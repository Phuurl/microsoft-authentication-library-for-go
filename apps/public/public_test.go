// Copyright (c) Microsoft Corporation.
// Licensed under the MIT license.

package public

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/internal/mock"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/internal/oauth/fake"
)

var tokenScope = []string{"the_scope"}

func fakeBrowserOpenURL(authURL string) error {
	// we will get called with the URL for requesting an auth code
	u, err := url.Parse(authURL)
	if err != nil {
		return err
	}
	// validate the URL content
	q := u.Query()
	if q.Get("code_challenge") == "" {
		return errors.New("missing query param 'code_challenge")
	}
	if m := q.Get("code_challenge_method"); m != "S256" {
		return fmt.Errorf("unexpected code_challenge_method '%s'", m)
	}
	if q.Get("prompt") == "" {
		return errors.New("missing query param 'prompt")
	}
	state := q.Get("state")
	if state == "" {
		return errors.New("missing query param 'state'")
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		return errors.New("missing query param 'redirect_uri'")
	}
	// now send the info to our local redirect server
	resp, err := http.DefaultClient.Get(redirect + fmt.Sprintf("/?state=%s&code=fake_auth_code", state))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}
	return nil
}

func TestAcquireTokenInteractive(t *testing.T) {
	realBrowserOpenURL := browserOpenURL
	defer func() { browserOpenURL = realBrowserOpenURL }()
	browserOpenURL = fakeBrowserOpenURL
	client, err := New("some_client_id")
	if err != nil {
		t.Fatal(err)
	}
	client.base.Token.AccessTokens = &fake.AccessTokens{}
	client.base.Token.Authority = &fake.Authority{}
	client.base.Token.Resolver = &fake.ResolveEndpoints{}
	client.base.Token.WSTrust = &fake.WSTrust{}
	_, err = client.AcquireTokenInteractive(context.Background(), []string{"the_scope"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcquireTokenSilentTenants(t *testing.T) {
	tenants := []string{"a", "b"}
	lmo := "login.microsoftonline.com"
	mockClient := mock.Client{}
	mockClient.AppendResponse(mock.WithBody(mock.GetInstanceDiscoveryBody(lmo, tenants[0])))
	client, err := New("client-id", WithHTTPClient(&mockClient))
	if err != nil {
		t.Fatal(err)
	}
	clientInfo := base64.RawStdEncoding.EncodeToString([]byte(`{"uid":"uid","utid":"utid"}`))
	ctx := context.Background()
	accounts := make([]Account, len(tenants))
	// cache an access token for each tenant. To simplify determining their provenance below, the value of each token is the ID of the tenant that provided it.
	for i, tenant := range tenants {
		if _, err = client.AcquireTokenSilent(ctx, tokenScope, WithTenantID(tenant)); err == nil {
			t.Fatal("silent auth should fail because the cache is empty")
		}
		mockClient.AppendResponse(mock.WithBody(mock.GetTenantDiscoveryBody(lmo, tenant)))
		mockClient.AppendResponse(mock.WithBody([]byte(`{"account_type":"Managed","cloud_audience_urn":"urn","cloud_instance_name":"...","domain_name":"..."}`)))
		mockClient.AppendResponse(mock.WithBody(
			mock.GetAccessTokenBody(tenant, mock.GetIDToken(tenant, fmt.Sprintf("https://%s/%s", lmo, tenant)), "rt-"+tenant, clientInfo, 3600)),
		)
		ar, err := client.AcquireTokenByUsernamePassword(ctx, tokenScope, "username", "password", WithTenantID(tenant))
		if err != nil {
			t.Fatal(err)
		}
		accounts[i] = ar.Account
	}
	// cache should return the correct access token for each tenant
	for i, account := range accounts {
		if account.Realm != tenants[i] {
			t.Fatalf(`unexpected realm "%s"`, account.Realm)
		}
		otherTenant := tenants[(i+1)%len(tenants)]
		for _, test := range []struct {
			desc, expected string
			opts           []AcquireSilentOption
		}{
			{"account only", account.Realm, []AcquireSilentOption{WithSilentAccount(account)}},
			{"matching account and tenant", account.Realm, []AcquireSilentOption{WithSilentAccount(account), WithTenantID(account.Realm)}},
			{"tenant overriding account", otherTenant, []AcquireSilentOption{WithSilentAccount(account), WithTenantID(otherTenant)}},
		} {
			t.Run(test.desc, func(t *testing.T) {
				ar, err := client.AcquireTokenSilent(ctx, tokenScope, test.opts...)
				if err != nil {
					t.Fatal(err)
				}
				if ar.AccessToken != test.expected {
					t.Fatalf(`expected "%s", got "%s"`, test.expected, ar.AccessToken)
				}
			})
		}
	}
}

func TestAcquireTokenWithTenantID(t *testing.T) {
	// replacing browserOpenURL with a fake for the duration of this test enables testing AcquireTokenInteractive
	realBrowserOpenURL := browserOpenURL
	defer func() { browserOpenURL = realBrowserOpenURL }()
	browserOpenURL = fakeBrowserOpenURL

	accessToken := "*"
	clientInfo := base64.RawStdEncoding.EncodeToString([]byte(`{"uid":"uid","utid":"utid"}`))
	uuid1 := "00000000-0000-0000-0000-000000000000"
	uuid2 := strings.ReplaceAll(uuid1, "0", "1")
	lmo := "login.microsoftonline.com"
	host := fmt.Sprintf("https://%s/", lmo)
	for _, test := range []struct {
		authority, expectedAuthority, tenant string
		expectError                          bool
	}{
		{authority: host + "common", tenant: uuid1, expectedAuthority: host + uuid1},
		{authority: host + "organizations", tenant: uuid1, expectedAuthority: host + uuid1},
		{authority: host + uuid1, tenant: uuid2, expectedAuthority: host + uuid2},
		{authority: host + uuid1, tenant: "common", expectError: true},
		{authority: host + uuid1, tenant: "organizations", expectError: true},
		{authority: host + "consumers", tenant: uuid1, expectError: true},
	} {
		for _, method := range []string{"authcode", "authcodeURL", "devicecode", "interactive", "password"} {
			t.Run(method, func(t *testing.T) {
				URL := ""
				mockClient := mock.Client{}
				if method == "obo" {
					// TODO: OBO does instance discovery twice before first token request https://github.com/AzureAD/microsoft-authentication-library-for-go/issues/351
					mockClient.AppendResponse(mock.WithBody(mock.GetInstanceDiscoveryBody(lmo, test.tenant)))
				}
				mockClient.AppendResponse(mock.WithBody(mock.GetInstanceDiscoveryBody(lmo, test.tenant)))
				mockClient.AppendResponse(mock.WithBody(mock.GetTenantDiscoveryBody(lmo, test.tenant)))
				if method == "devicecode" {
					mockClient.AppendResponse(mock.WithBody([]byte(`{"device_code":"...","expires_in":600}`)))
				} else if method == "password" {
					// user realm metadata
					mockClient.AppendResponse(mock.WithBody([]byte(`{"account_type":"Managed","cloud_audience_urn":"urn","cloud_instance_name":"...","domain_name":"..."}`)))
				}
				mockClient.AppendResponse(
					mock.WithBody(mock.GetAccessTokenBody(accessToken, mock.GetIDToken(test.tenant, test.authority), "rt", clientInfo, 3600)),
					mock.WithCallback(func(r *http.Request) { URL = r.URL.String() }),
				)
				client, err := New("client-id", WithAuthority(test.authority), WithHTTPClient(&mockClient))
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if _, err = client.AcquireTokenSilent(ctx, tokenScope, WithTenantID(test.tenant)); err == nil {
					t.Fatal("silent auth should fail because the cache is empty")
				}

				var ar AuthResult
				var dc DeviceCode
				switch method {
				case "authcode":
					ar, err = client.AcquireTokenByAuthCode(ctx, "auth code", "https://localhost", tokenScope, WithTenantID(test.tenant))
				case "authcodeURL":
					URL, err = client.CreateAuthCodeURL(ctx, "client-id", "https://localhost", tokenScope, WithTenantID(test.tenant))
				case "devicecode":
					dc, err = client.AcquireTokenByDeviceCode(ctx, tokenScope, WithTenantID(test.tenant))
				case "interactive":
					ar, err = client.AcquireTokenInteractive(ctx, tokenScope, WithTenantID(test.tenant))
				case "password":
					ar, err = client.AcquireTokenByUsernamePassword(ctx, tokenScope, "username", "password", WithTenantID(test.tenant))
				default:
					t.Fatalf("no test for " + method)
				}
				if err != nil {
					if test.expectError {
						return
					}
					t.Fatal(err)
				} else if test.expectError {
					t.Fatal("expected an error")
				}
				if method == "devicecode" {
					if ar, err = dc.AuthenticationResult(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if !strings.HasPrefix(URL, test.expectedAuthority) {
					t.Fatalf(`expected "%s", got "%s"`, test.expectedAuthority, URL)
				}
				if method == "authcodeURL" {
					// didn't acquire a token, no need to test silent auth
					return
				}
				if ar.AccessToken != accessToken {
					t.Fatalf(`unexpected access token "%s"`, ar.AccessToken)
				}
				// silent authentication should succeed for the given tenant...
				if ar, err = client.AcquireTokenSilent(ctx, tokenScope, WithSilentAccount(ar.Account), WithTenantID(test.tenant)); err != nil {
					t.Fatal(err)
				} else if ar.AccessToken != accessToken {
					t.Fatal("cached access token should match the one returned by AcquireToken...")
				}
				// ...but fail for another tenant
				if _, err = client.AcquireTokenSilent(ctx, tokenScope, WithSilentAccount(ar.Account), WithTenantID("not-"+test.tenant)); err == nil {
					t.Fatal("expected an error")
				}
			})
		}
	}
}

func TestWithLoginHint(t *testing.T) {
	realBrowserOpenURL := browserOpenURL
	defer func() { browserOpenURL = realBrowserOpenURL }()
	upn := "user@localhost"
	client, err := New("client-id")
	if err != nil {
		t.Fatal(err)
	}
	client.base.Token.AccessTokens = &fake.AccessTokens{}
	client.base.Token.Authority = &fake.Authority{}
	client.base.Token.Resolver = &fake.ResolveEndpoints{}
	for _, expectHint := range []bool{true, false} {
		t.Run(fmt.Sprint(expectHint), func(t *testing.T) {
			// replace the browser launching function with a fake that validates login_hint is set as expected
			called := false
			validate := func(v url.Values) error {
				if !v.Has("login_hint") {
					if !expectHint {
						return nil
					}
					return errors.New("expected a login hint")
				} else if !expectHint {
					return errors.New("expected no login hint")
				}
				if actual := v["login_hint"]; len(actual) != 1 || actual[0] != upn {
					err = fmt.Errorf(`unexpected login_hint "%v"`, actual)
				}
				return err
			}
			browserOpenURL = func(authURL string) error {
				called = true
				parsed, err := url.Parse(authURL)
				if err != nil {
					return err
				}
				query, err := url.ParseQuery(parsed.RawQuery)
				if err != nil {
					return err
				}
				if err = validate(query); err != nil {
					t.Fatal(err)
					return err
				}
				// this helper validates the other params and completes the redirect
				return fakeBrowserOpenURL(authURL)
			}
			acquireOpts := []AcquireInteractiveOption{}
			urlOpts := []CreateAuthCodeURLOption{}
			if expectHint {
				acquireOpts = append(acquireOpts, WithLoginHint(upn))
				urlOpts = append(urlOpts, WithLoginHint(upn))
			}
			_, err = client.AcquireTokenInteractive(context.Background(), tokenScope, acquireOpts...)
			if err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("browserOpenURL wasn't called")
			}
			u, err := client.CreateAuthCodeURL(context.Background(), "id", "https://localhost", tokenScope, urlOpts...)
			if err == nil {
				var parsed *url.URL
				parsed, err = url.Parse(u)
				if err == nil {
					err = validate(parsed.Query())
				}
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
