package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/cap/oidc"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/builtin/credential/userpass"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/hashicorp/vault/vault"
	"github.com/mitchellh/pointerstructure"
	"github.com/stretchr/testify/require"
)

const (
	testPassword           = "testpassword"
	testRedirectURI        = "https://127.0.0.1:8251/callback"
	testGroupScopeTemplate = `
		{
			"groups": {{identity.entity.groups.names}}
		}
	`
	testUserScopeTemplate = `
		{
			"username": {{identity.entity.aliases.%s.name}},
			"contact": {
				"email": {{identity.entity.metadata.email}},
				"phone_number": {{identity.entity.metadata.phone_number}}
			}
		}
	`
)

// TestOIDC_Auth_Code_Flow_CAP_Client tests the authorization code flow
// using a Vault OIDC provider. The test uses the CAP OIDC client to verify
// that the Vault OIDC provider's responses pass the various client-side
// validation requirements of the OIDC spec.
func TestOIDC_Auth_Code_Flow_CAP_Client(t *testing.T) {
	cluster := setupOIDCTestCluster(t, 2)
	active := cluster.Cores[0].Client
	standby := cluster.Cores[1].Client

	// Create an entity with some metadata
	resp, err := active.Logical().Write("identity/entity", map[string]interface{}{
		"name": "test-entity",
		"metadata": map[string]string{
			"email":        "test@hashicorp.com",
			"phone_number": "123-456-7890",
		},
	})
	require.NoError(t, err)
	entityID := resp.Data["id"].(string)

	// Create a group
	resp, err = active.Logical().Write("identity/group", map[string]interface{}{
		"name":              "engineering",
		"member_entity_ids": []string{entityID},
	})
	require.NoError(t, err)
	groupID := resp.Data["id"].(string)

	// Enable userpass auth and create a user
	err = active.Sys().EnableAuthWithOptions("userpass", &api.EnableAuthOptions{
		Type: "userpass",
	})
	require.NoError(t, err)
	_, err = active.Logical().Write("auth/userpass/users/end-user", map[string]interface{}{
		"password": testPassword,
	})
	require.NoError(t, err)

	// Get the userpass mount accessor
	mounts, err := active.Sys().ListAuth()
	require.NoError(t, err)
	var mountAccessor string
	for k, v := range mounts {
		if k == "userpass/" {
			mountAccessor = v.Accessor
			break
		}
	}
	require.NotEmpty(t, mountAccessor)

	// Create an entity alias
	_, err = active.Logical().Write("identity/entity-alias", map[string]interface{}{
		"name":           "end-user",
		"canonical_id":   entityID,
		"mount_accessor": mountAccessor,
	})
	require.NoError(t, err)

	// Create some custom scopes
	_, err = active.Logical().Write("identity/oidc/scope/groups", map[string]interface{}{
		"template": testGroupScopeTemplate,
	})
	require.NoError(t, err)
	_, err = active.Logical().Write("identity/oidc/scope/user", map[string]interface{}{
		"template": fmt.Sprintf(testUserScopeTemplate, mountAccessor),
	})
	require.NoError(t, err)

	// Create a key
	_, err = active.Logical().Write("identity/oidc/key/test-key", map[string]interface{}{
		"allowed_client_ids": []string{"*"},
		"algorithm":          "RS256",
	})
	require.NoError(t, err)

	// Create an assignment
	_, err = active.Logical().Write("identity/oidc/assignment/test-assignment", map[string]interface{}{
		"entity_ids": []string{entityID},
		"group_ids":  []string{groupID},
	})
	require.NoError(t, err)

	// Create a client
	_, err = active.Logical().Write("identity/oidc/client/test-client", map[string]interface{}{
		"key":              "test-key",
		"redirect_uris":    []string{testRedirectURI},
		"assignments":      []string{"test-assignment"},
		"id_token_ttl":     "24h",
		"access_token_ttl": "24h",
	})
	require.NoError(t, err)

	// Read the client ID and secret in order to configure the OIDC client
	resp, err = active.Logical().Read("identity/oidc/client/test-client")
	require.NoError(t, err)
	clientID := resp.Data["client_id"].(string)
	clientSecret := resp.Data["client_secret"].(string)

	// Create the OIDC provider
	_, err = active.Logical().Write("identity/oidc/provider/test-provider", map[string]interface{}{
		"allowed_client_ids": []string{clientID},
		"scopes":             []string{"user", "groups"},
	})
	require.NoError(t, err)

	// We aren't going to open up a browser to facilitate the login and redirect
	// from this test, so we'll log in via userpass and set the client's token as
	// the token that results from the authentication.
	resp, err = active.Logical().Write("auth/userpass/login/end-user", map[string]interface{}{
		"password": testPassword,
	})
	require.NoError(t, err)
	active.SetToken(resp.Auth.ClientToken)
	standby.SetToken(resp.Auth.ClientToken)

	// Read the issuer from the OIDC provider's discovery document
	var discovery struct {
		Issuer string `json:"issuer"`
	}
	decodeRawRequest(t, active, http.MethodGet,
		"/v1/identity/oidc/provider/test-provider/.well-known/openid-configuration",
		nil, &discovery)

	// Create the client-side OIDC provider config
	pc, err := oidc.NewConfig(discovery.Issuer, clientID,
		oidc.ClientSecret(clientSecret), []oidc.Alg{oidc.RS256},
		[]string{testRedirectURI}, oidc.WithProviderCA(string(cluster.CACertPEM)))
	require.NoError(t, err)

	// Create the client-side OIDC provider
	p, err := oidc.NewProvider(pc)
	require.NoError(t, err)
	defer p.Done()

	type expectedClaim struct {
		key   string
		value interface{}
	}
	type args struct {
		useStandby bool
		options    []oidc.Option
		expected   []expectedClaim
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "active: authorization code flow",
			args: args{
				options: []oidc.Option{
					oidc.WithScopes("openid user"),
				},
				expected: []expectedClaim{
					{
						key:   "username",
						value: "end-user",
					},
					{
						key:   "/contact/email",
						value: "test@hashicorp.com",
					},
					{
						key:   "/contact/phone_number",
						value: "123-456-7890",
					},
				},
			},
		},
		{
			name: "active: authorization code flow with additional scopes",
			args: args{
				options: []oidc.Option{
					oidc.WithScopes("openid user groups"),
				},
				expected: []expectedClaim{
					{
						key:   "username",
						value: "end-user",
					},
					{
						key:   "/contact/email",
						value: "test@hashicorp.com",
					},
					{
						key:   "/contact/phone_number",
						value: "123-456-7890",
					},
					{
						key:   "groups",
						value: []interface{}{"engineering"},
					},
				},
			},
		},
		{
			name: "standby: authorization code flow with additional scopes",
			args: args{
				useStandby: true,
				options: []oidc.Option{
					oidc.WithScopes("openid user groups"),
				},
				expected: []expectedClaim{
					{
						key:   "username",
						value: "end-user",
					},
					{
						key:   "/contact/email",
						value: "test@hashicorp.com",
					},
					{
						key:   "/contact/phone_number",
						value: "123-456-7890",
					},
					{
						key:   "groups",
						value: []interface{}{"engineering"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := active
			if tt.args.useStandby {
				cl = standby
			}

			// Create the client-side OIDC request state
			oidcRequest, err := oidc.NewRequest(10*time.Minute, testRedirectURI,
				oidc.WithScopes("openid user groups"))
			require.NoError(t, err)

			// Get the URL for the authorization endpoint from the OIDC client
			authURL, err := p.AuthURL(context.Background(), oidcRequest)
			require.NoError(t, err)
			parsedAuthURL, err := url.Parse(authURL)
			require.NoError(t, err)

			// This replace only occurs because we're not using the browser in this test
			authURLPath := strings.Replace(parsedAuthURL.Path, "/ui/vault/", "/v1/", 1)

			// Kick off the authorization code flow
			var authResp struct {
				Code  string `json:"code"`
				State string `json:"state"`
			}
			decodeRawRequest(t, cl, http.MethodGet, authURLPath, parsedAuthURL.Query(), &authResp)

			// The returned state must match the OIDC client state
			require.Equal(t, oidcRequest.State(), authResp.State)

			// Exchange the authorization code for an ID token and access token.
			// The ID token signature is verified using the provider's public keys after
			// the exchange takes place. The ID token is also validated according to the
			// client-side requirements of the OIDC spec. See the validation code at:
			// - https://github.com/hashicorp/cap/blob/main/oidc/provider.go#L240
			// - https://github.com/hashicorp/cap/blob/main/oidc/provider.go#L441
			token, err := p.Exchange(context.Background(), oidcRequest, authResp.State, authResp.Code)
			require.NoError(t, err)
			require.NotNil(t, token)
			idToken := token.IDToken()
			accessToken := token.StaticTokenSource()

			// Get the ID token claims
			allClaims := make(map[string]interface{})
			require.NoError(t, idToken.Claims(&allClaims))

			// Get the sub claim for userinfo validation
			require.NotEmpty(t, allClaims["sub"])
			subject := allClaims["sub"].(string)

			// Request userinfo using the access token
			err = p.UserInfo(context.Background(), accessToken, subject, &allClaims)
			require.NoError(t, err)

			// Assert that all required claims are present as top-level keys
			requiredClaims := []string{
				"iat", "aud", "exp", "iss", "sub",
				"namespace", "nonce", "at_hash", "c_hash",
			}
			for _, required := range requiredClaims {
				require.NotEmpty(t, getClaim(t, allClaims, required))
			}

			// Assert that claims given by the scope templates are populated
			for _, expected := range tt.args.expected {
				actualValue := getClaim(t, allClaims, expected.key)
				require.Equal(t, expected.value, actualValue)
			}
		})
	}
}

func setupOIDCTestCluster(t *testing.T, numCores int) *vault.TestCluster {
	t.Helper()

	coreConfig := &vault.CoreConfig{
		CredentialBackends: map[string]logical.Factory{
			"userpass": userpass.Factory,
		},
	}
	clusterOptions := &vault.TestClusterOptions{
		NumCores:    numCores,
		HandlerFunc: vaulthttp.Handler,
	}
	cluster := vault.NewTestCluster(t, coreConfig, clusterOptions)
	cluster.Start()
	vault.TestWaitActive(t, cluster.Cores[0].Core)

	return cluster
}

func decodeRawRequest(t *testing.T, client *api.Client, method, path string, params url.Values, v interface{}) {
	t.Helper()

	// Create the request and add query params if provided
	req := client.NewRequest(method, path)
	req.Params = params

	// Send the raw request
	r, err := client.RawRequest(req)
	require.NoError(t, err)
	require.NotNil(t, r)
	require.Equal(t, http.StatusOK, r.StatusCode)
	defer r.Body.Close()

	// Decode the body into v
	require.NoError(t, json.NewDecoder(r.Body).Decode(v))
}

// getClaim returns a claim value from claims given a provided claim string.
// If this string is a valid JSON Pointer, it will be interpreted as such to
// locate the claim. Otherwise, the claim string will be used directly.
func getClaim(t *testing.T, claims map[string]interface{}, claim string) interface{} {
	t.Helper()

	if !strings.HasPrefix(claim, "/") {
		return claims[claim]
	}

	val, err := pointerstructure.Get(claims, claim)
	require.NoError(t, err)
	return val
}
