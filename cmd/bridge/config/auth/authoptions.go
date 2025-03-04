package auth

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	"github.com/openshift/console/pkg/auth"
	"github.com/openshift/console/pkg/flags"
	"github.com/openshift/console/pkg/proxy"
	"github.com/openshift/console/pkg/server"
	"github.com/openshift/console/pkg/serverconfig"
)

type AuthOptions struct {
	AuthType string

	IssuerURL            string
	ClientID             string
	ClientSecret         string
	ClientSecretFilePath string
	CAFilePath           string

	InactivityTimeoutSeconds int
	LogoutRedirect           string
}

type CompletedOptions struct {
	*completedOptions
}

type completedOptions struct {
	AuthType string

	IssuerURL    *url.URL
	ClientID     string
	ClientSecret string
	CAFilePath   string

	InactivityTimeoutSeconds int
	LogoutRedirectURL        *url.URL
}

func NewAuthOptions() *AuthOptions {
	return &AuthOptions{}
}

func (c *AuthOptions) AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.AuthType, "user-auth", "", "User authentication provider type. Possible values: disabled, oidc, openshift. Defaults to 'openshift'")
	fs.StringVar(&c.IssuerURL, "user-auth-oidc-issuer-url", "", "The OIDC/OAuth2 issuer URL.")
	fs.StringVar(&c.ClientID, "user-auth-oidc-client-id", "", "The OIDC OAuth2 Client ID.")
	fs.StringVar(&c.ClientSecret, "user-auth-oidc-client-secret", "", "The OIDC OAuth2 Client Secret.")
	fs.StringVar(&c.ClientSecretFilePath, "user-auth-oidc-client-secret-file", "", "File containing the OIDC OAuth2 Client Secret.")
	fs.StringVar(&c.CAFilePath, "user-auth-oidc-ca-file", "", "Path to a PEM file for the OIDC/OAuth2 issuer CA.")

	fs.IntVar(&c.InactivityTimeoutSeconds, "inactivity-timeout", 0, "Number of seconds, after which user will be logged out if inactive. Ignored if less than 300 seconds (5 minutes).")
	fs.StringVar(&c.LogoutRedirect, "user-auth-logout-redirect", "", "Optional redirect URL on logout needed for some single sign-on identity providers.")
}

func (c *AuthOptions) ApplyConfig(config *serverconfig.Auth) {
	setIfUnset(&c.ClientID, config.ClientID)
	setIfUnset(&c.ClientSecretFilePath, config.ClientSecretFile)
	setIfUnset(&c.CAFilePath, config.OAuthEndpointCAFile)
	setIfUnset(&c.LogoutRedirect, config.LogoutRedirect)

	if c.InactivityTimeoutSeconds == 0 {
		c.InactivityTimeoutSeconds = config.InactivityTimeoutSeconds
	}
}

func (c *AuthOptions) Complete(k8sAuthType string) (*CompletedOptions, error) {
	// default values before running validation
	if len(c.AuthType) == 0 {
		c.AuthType = "openshift"
	}

	if c.InactivityTimeoutSeconds < 300 {
		klog.Warning("Flag inactivity-timeout is set to less then 300 seconds and will be ignored!")
		c.InactivityTimeoutSeconds = 0
	}

	if errs := c.Validate(k8sAuthType); len(errs) > 0 {
		return nil, utilerrors.NewAggregate(errs)
	}

	completed := &completedOptions{
		AuthType:                 c.AuthType,
		ClientID:                 c.ClientID,
		ClientSecret:             c.ClientSecret,
		CAFilePath:               c.CAFilePath,
		InactivityTimeoutSeconds: c.InactivityTimeoutSeconds,
	}

	if len(c.IssuerURL) > 0 {
		issuerURL, err := url.Parse(c.IssuerURL)
		if err != nil {
			return nil, fmt.Errorf("invalid issuer URL: %w", err)
		}
		completed.IssuerURL = issuerURL
	}

	if len(c.LogoutRedirect) > 0 {
		logoutURL, err := url.Parse(c.LogoutRedirect)
		if err != nil {
			return nil, fmt.Errorf("invalid logout redirect URL: %w", err)
		}
		completed.LogoutRedirectURL = logoutURL
	}

	if len(c.ClientSecretFilePath) > 0 {
		buf, err := os.ReadFile(c.ClientSecretFilePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read client secret file: %w", err)
		}
		completed.ClientSecret = string(buf)
	}

	return &CompletedOptions{
		completedOptions: completed,
	}, nil
}

func (c *AuthOptions) Validate(k8sAuthType string) []error {
	var errs []error

	switch c.AuthType {
	case "openshift", "oidc":
		if len(c.ClientID) == 0 {
			errs = append(errs, flags.NewRequiredFlagError("user-auth-oidc-client-id"))
		}

		if c.ClientSecret == "" && c.ClientSecretFilePath == "" {
			errs = append(errs, fmt.Errorf("must provide either --user-auth-oidc-client-secret or --user-auth-oidc-client-secret-file"))
		}

		if c.ClientSecret != "" && c.ClientSecretFilePath != "" {
			errs = append(errs, fmt.Errorf("cannot provide both --user-auth-oidc-client-secret and --user-auth-oidc-client-secret-file"))
		}

	case "disabled":
	default:
		errs = append(errs, flags.NewInvalidFlagError("user-auth", "must be one of: oidc, openshift, disabled"))
	}

	switch c.AuthType {
	case "openshift":
		if len(c.IssuerURL) != 0 {
			errs = append(errs, flags.NewInvalidFlagError("user-auth-oidc-issuer-url", "cannot be used with --user-auth=\"openshift\""))
		}

	case "oidc":
		if len(c.IssuerURL) == 0 {
			errs = append(errs, fmt.Errorf("--user-auth-oidc-issuer-url must be set if --user-auth=oidc"))
		}
	}

	switch k8sAuthType {
	case "oidc", "openshift":
	default:
		if c.InactivityTimeoutSeconds > 0 {
			errs = append(errs, flags.NewInvalidFlagError("inactivity-timeout", "in order to activate the user inactivity timout, flag --user-auth must be one of: oidc, openshift"))
		}
	}

	return errs
}

func (c *completedOptions) ApplyTo(
	srv *server.Server,
	k8sEndpoint *url.URL,
	pubAPIServerEndpoint string,
	caCertFilePath string,
) error {
	srv.InactivityTimeout = c.InactivityTimeoutSeconds
	srv.LogoutRedirect = c.LogoutRedirectURL

	var err error
	srv.Authenticator, err = c.getAuthenticator(
		srv.BaseURL,
		k8sEndpoint,
		pubAPIServerEndpoint,
		caCertFilePath,
		srv.K8sClient.Transport,
	)

	return err
}

func (c *completedOptions) getAuthenticator(
	baseURL *url.URL,
	k8sEndpoint *url.URL,
	pubAPIServerEndpoint string,
	caCertFilePath string,
	k8sTransport http.RoundTripper,
) (*auth.Authenticator, error) {

	if c.AuthType == "disabled" {
		klog.Warning("running with AUTHENTICATION DISABLED!")
		return nil, nil
	}

	flags.FatalIfFailed(flags.ValidateFlagNotEmpty("base-address", baseURL.String()))

	var (
		err                      error
		userAuthOIDCIssuerURL    *url.URL
		authLoginErrorEndpoint   = proxy.SingleJoiningSlash(baseURL.String(), server.AuthLoginErrorEndpoint)
		authLoginSuccessEndpoint = proxy.SingleJoiningSlash(baseURL.String(), server.AuthLoginSuccessEndpoint)
		oidcClientSecret         = c.ClientSecret
		// Abstraction leak required by NewAuthenticator. We only want the browser to send the auth token for paths starting with basePath/api.
		cookiePath       = proxy.SingleJoiningSlash(baseURL.Path, "/api/")
		refererPath      = baseURL.String()
		useSecureCookies = baseURL.Scheme == "https"
	)

	scopes := []string{"openid", "email", "profile", "groups"}
	authSource := auth.AuthSourceTectonic

	if c.AuthType == "openshift" {
		// Scopes come from OpenShift documentation
		// https://access.redhat.com/documentation/en-us/openshift_container_platform/4.9/html/authentication_and_authorization/using-service-accounts-as-oauth-client
		//
		// TODO(ericchiang): Support other scopes like view only permissions.
		scopes = []string{"user:full"}
		authSource = auth.AuthSourceOpenShift

		userAuthOIDCIssuerURL = k8sEndpoint
	} else {
		userAuthOIDCIssuerURL = c.IssuerURL

	}

	oidcClientSecret = c.ClientSecret

	// Config for logging into console.
	oidcClientConfig := &auth.Config{
		AuthSource:   authSource,
		IssuerURL:    userAuthOIDCIssuerURL.String(),
		IssuerCA:     c.CAFilePath,
		ClientID:     c.ClientID,
		ClientSecret: oidcClientSecret,
		RedirectURL:  proxy.SingleJoiningSlash(baseURL.String(), server.AuthLoginCallbackEndpoint),
		Scope:        scopes,

		// Use the k8s CA file for OpenShift OAuth metadata discovery.
		// This might be different than IssuerCA.
		K8sCA: caCertFilePath,

		ErrorURL:   authLoginErrorEndpoint,
		SuccessURL: authLoginSuccessEndpoint,

		CookiePath:    cookiePath,
		RefererPath:   refererPath,
		SecureCookies: useSecureCookies,

		K8sConfig: &rest.Config{
			Host:      pubAPIServerEndpoint,
			Transport: k8sTransport,
		},
	}

	authenticator, err := auth.NewAuthenticator(context.Background(), oidcClientConfig)
	if err != nil {
		klog.Fatalf("Error initializing authenticator: %v", err)
	}

	return authenticator, nil
}

func setIfUnset(flagVal *string, val string) {
	if len(*flagVal) == 0 {
		*flagVal = val
	}
}
