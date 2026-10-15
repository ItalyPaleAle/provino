package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/italypaleale/provino/imagekeys"
	"github.com/italypaleale/provino/internal/sniff"
	"github.com/italypaleale/provino/variant"
)

// Topologies the Francis actor runtime can run in
const (
	// ActorRuntimeEmbedded runs the actor runtime inside this process, so there's no control plane to deploy alongside the service
	ActorRuntimeEmbedded = "embedded"
	// ActorRuntimeExternal connects to a standalone Francis runtime, which owns placement and durable job and alarm scheduling for the whole cluster
	ActorRuntimeExternal = "external"
)

// Kinds of authorizer that decide what a browser upload may do
const (
	// AuthorizerHTTP asks an HTTP endpoint the operator runs
	AuthorizerHTTP = "http"
	// AuthorizerStatic applies the same decision, from the configuration, to every upload
	AuthorizerStatic = "static"
)

// Ways this service authenticates the requests it sends: webhook deliveries and authorizer calls
const (
	OutboundAuthHMAC    = "hmac"
	OutboundAuthEd25519 = "ed25519"
	OutboundAuthOAuth2  = "oauth2"
	OutboundAuthNone    = "none"
)

// APIKeyPrefix starts every API key, which is what tells one apart from a JWT in the Authorization header
const APIKeyPrefix = "pvk_"

// EnvActorsAddress names the environment variable that overrides 'actors.address'
// Instances that share one configuration file each need their own address, which only the environment can give them
const EnvActorsAddress = "PROVINO_ACTORS_ADDRESS"

// Name of the folder inside the system temporary directory the source images are cached in when 'storage.sourceCache' is not set
const defaultSourceCacheDirName = "provino-sources"

// Names of webhook endpoints and API keys, which appear in logs and in requests
var nameRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Config represents the application configuration
type Config struct {
	// Server configuration
	Server ConfigServer `yaml:"server"`

	// Auth configures how callers authenticate to this service
	Auth ConfigAuth `yaml:"auth"`

	// Authorizer decides where a browser upload goes and what it may produce
	// Browser uploads to `/v1/public/ingest` are disabled when no authorizer is configured
	Authorizer ConfigAuthorizer `yaml:"authorizer"`

	// Notify configures the webhook endpoints jobs report their outcome to
	Notify ConfigNotify `yaml:"notify"`

	// Signing configures the Ed25519 keys this service signs requests with
	Signing ConfigSigning `yaml:"signing"`

	// Images contains the limits for the images this service accepts and the variants it generates
	Images ConfigImages `yaml:"images"`

	// Storage contains the configuration for where the source images and the generated variants are stored
	Storage ConfigStorage `yaml:"storage"`

	// RateLimit configures the per-caller limit on browser uploads
	RateLimit ConfigRateLimit `yaml:"rateLimit"`

	// Actors contains the configuration for the Francis actor runtime that runs the durable jobs
	Actors ConfigActors `yaml:"actors"`

	// Logs contains configuration for logging
	Logs ConfigLogs `yaml:"logs"`

	// Dev is meant for development only; it's undocumented
	Dev ConfigDev `yaml:"-"`

	// Internal keys
	internal internal `yaml:"-"`
}

// ConfigServer represents server configuration
type ConfigServer struct {
	// Address to bind the API server to
	// Defaults to all interfaces because the service is meant to run in a container, where the port is published rather than reached over loopback
	// Set to "127.0.0.1" to only accept connections from the same host
	// +default "0.0.0.0"
	Bind string `yaml:"bind"`

	// Port for the API server to listen on
	// +default 3000
	Port int `yaml:"port"`

	// Public origin of this service
	// It's the default audience of the JWTs this service accepts
	// +default "http://127.0.0.1:3000"
	PublicURL string `yaml:"publicUrl"`

	// Origins browsers may call `/v1/public/*` from, for example `https://app.example.com`
	// Only the public routes answer cross-origin requests; the client API never does
	CORSAllowedOrigins []string `yaml:"corsAllowedOrigins"`

	// IP addresses or CIDRs of reverse proxies whose X-Forwarded-For and X-Real-IP headers can identify rate-limit clients
	TrustedProxies []string `yaml:"trustedProxies"`

	// Maximum time to read a whole request, including its body
	// It bounds how long a slow client can hold an upload open, so it has to leave room for the largest upload over the slowest link you accept
	// +default "5m"
	ReadTimeout time.Duration `yaml:"readTimeout"`

	// Maximum time to write a response, measured from the end of reading the request headers
	// +default "5m"
	WriteTimeout time.Duration `yaml:"writeTimeout"`

	// TLS configuration
	TLS ConfigServerTLS `yaml:"tls"`
}

// ConfigServerTLS holds TLS configuration for TCP listener
type ConfigServerTLS struct {
	// Path where to load TLS certificates from
	// Within the folder, the files must be named `tls-cert.pem` and `tls-key.pem`. The application watches for changes in this folder and automatically reloads the TLS certificates when they're updated.
	// If empty, certificates are loaded from the same folder where the loaded `config.yaml` is located.
	// +default the same folder as the `config.yaml` file
	Path string `yaml:"path"`

	// Full, PEM-encoded TLS certificate
	// Using `certPEM` and `keyPEM` is an alternative method of passing TLS certificates than using `path`.
	CertPEM string `yaml:"certPEM"` //nolint:tagliatelle

	// Full, PEM-encoded TLS key
	// Using `certPEM` and `keyPEM` is an alternative method of passing TLS certificates than using `path`.
	KeyPEM string `yaml:"keyPEM"` //nolint:tagliatelle
}

// ConfigAuth configures how callers authenticate to this service
//
// JWTs and API keys can both be enabled; `none` replaces them both and is meant for development only
type ConfigAuth struct {
	// JWT configures verification of bearer tokens issued by an OpenID Connect provider
	JWT ConfigAuthJWT `yaml:"jwt"`

	// API keys accepted in the `Authorization: Bearer` header
	// Each entry has an `id`, exactly one of `key` (the key itself, which must start with `pvk_`) and `sha256` (the hex-encoded SHA-256 of the key), the `subject` the key authenticates as (defaulting to the `id`), and the `scopes` it grants
	// +example [{"id": "backend", "sha256": "…", "scopes": ["server"]}]
	APIKeys []ConfigAPIKey `yaml:"apiKeys"`

	// Scopes a caller needs for each part of the API
	Scopes ConfigAuthScopes `yaml:"scopes"`

	// None disables authentication, which is meant for development only
	None ConfigAuthNone `yaml:"none"`
}

// ConfigAuthJWT configures verification of JWTs
type ConfigAuthJWT struct {
	// Issuer whose tokens are accepted, which must match their `iss` claim
	// When `jwksUrl` is empty, the key set is discovered from `<issuer>/.well-known/openid-configuration`
	// JWT authentication is disabled when this is empty
	// +example "https://tenant.example.com/"
	Issuer string `yaml:"issuer"`

	// Expected `aud` claim, which is this service's identifier at the provider
	// If empty, `server.publicUrl` is used
	Audience string `yaml:"audience"`

	// URL of the issuer's JWKS, for providers that don't publish OpenID Connect discovery
	// A key set found through discovery must be on the issuer's origin, but one named here is trusted as the operator configured it
	JWKSURL string `yaml:"jwksUrl"`

	// Claim that holds the caller's subject
	// +default "sub"
	SubjectClaim string `yaml:"subjectClaim"`

	// Claim that holds the caller's scopes, either as a space-separated string or as an array
	// +default "scope"
	ScopeClaim string `yaml:"scopeClaim"`
}

// ConfigAPIKey is one API key callers can authenticate with
type ConfigAPIKey struct {
	// Name of the key, which appears in logs
	ID string `yaml:"id"`
	// The key itself, which must start with `pvk_`
	Key string `yaml:"key"`
	// Hex-encoded SHA-256 of the key, as an alternative to `key` that keeps the key out of the configuration
	SHA256 string `yaml:"sha256"`
	// Subject the key authenticates as, defaulting to the `id`
	Subject string `yaml:"subject"`
	// Scopes the key grants
	Scopes []string `yaml:"scopes"`
}

// ConfigAuthScopes names the scope each part of the API requires
type ConfigAuthScopes struct {
	// Scope required to upload through `/v1/public/ingest` and read the status of one's own jobs
	// Set it to an empty string to accept any authenticated caller
	// +default "ingest"
	Ingest string `yaml:"ingest"`

	// Scope required to use the client API under `/v1/client`, which is trusted with every namespace
	// Set it to an empty string to accept any authenticated caller
	// +default "server"
	Server string `yaml:"server"`
}

// ConfigAuthNone disables authentication
type ConfigAuthNone struct {
	// If true, every request is accepted as `subject` with `scopes`, without checking any credentials
	// It can't be combined with `jwt` or `apiKeys`, and it's meant for development only
	Enabled bool `yaml:"enabled"`
	// Subject every request is accepted as
	// +default "anonymous"
	Subject string `yaml:"subject"`
	// Scopes every request is granted
	// +default ["ingest", "server"]
	Scopes []string `yaml:"scopes"`
}

// ConfigAuthorizer decides where a browser upload goes and what it may produce
type ConfigAuthorizer struct {
	// Kind of authorizer: `http` or `static`
	// If empty, browser uploads are disabled
	Type string `yaml:"type"`

	// HTTP configures the authorizer endpoint, used when `type` is `http`
	HTTP ConfigAuthorizerHTTP `yaml:"http"`

	// Static configures the decision applied to every upload, used when `type` is `static`
	Static ConfigAuthorizerStatic `yaml:"static"`
}

// ConfigAuthorizerHTTP configures the authorizer endpoint
type ConfigAuthorizerHTTP struct {
	// URL the authorization request is posted to
	// +example "https://app.example.com/api/provino/authorize"
	URL string `yaml:"url"`

	// Maximum time to wait for the authorizer's answer
	// +default "10s"
	Timeout time.Duration `yaml:"timeout"`

	// How the request authenticates to the authorizer, with the same options as a webhook endpoint's `auth`
	Auth ConfigOutboundAuth `yaml:"auth"`
}

// ConfigAuthorizerStatic is the decision the static authorizer applies to every upload
type ConfigAuthorizerStatic struct {
	// Namespace every upload is stored in
	Namespace string `yaml:"namespace"`

	// Maximum size of an upload in bytes, applied together with `images.maxUploadSize`
	MaxBytes int64 `yaml:"maxBytes"`

	// Source formats uploads may be in: `jpg`, `png`, `webp`, `avif`, `heic`, `heif`
	// If empty, every supported format is allowed
	AllowedFormats []string `yaml:"allowedFormats"`

	// Variants to generate for every upload, each with `format`, `quality`, optional `effort`, `size` (`width` and `height`), and optional `fit`
	// +example [{"format": "webp", "quality": 80, "size": {"width": 1200, "height": 1200}}]
	Variants []variant.Spec `yaml:"variants"`

	// Where each upload's job reports its outcome, with the name of an `endpoint` and an optional `context` object
	Notify *ConfigNotifyTarget `yaml:"notify"`
}

// ConfigNotifyTarget names a webhook endpoint and the context its events carry
type ConfigNotifyTarget struct {
	// Name of the endpoint in `notify.endpoints`, or empty for the default one
	Endpoint string `yaml:"endpoint"`
	// Object echoed back in every event
	Context map[string]any `yaml:"context"`
}

// ConfigNotify configures the webhook endpoints jobs report their outcome to
//
// Requests and authorizers pick an endpoint by name, so no URL ever comes from a caller
type ConfigNotify struct {
	// Endpoint used when a request asks for a notification without naming one
	// If empty and exactly one endpoint is configured, that one is used
	DefaultEndpoint string `yaml:"defaultEndpoint"`

	// Webhook endpoints by name
	// Each one has a `url`, an optional `timeout` (default `15s`), and the `auth` its requests carry, with the same options as `authorizer.http.auth`
	// +example {"app": {"url": "https://app.example.com/hooks/provino", "auth": {"type": "hmac", "hmac": {"secrets": ["whsec_…"]}}}}
	Endpoints map[string]ConfigNotifyEndpoint `yaml:"endpoints"`
}

// ConfigNotifyEndpoint is one webhook endpoint
type ConfigNotifyEndpoint struct {
	// URL events are posted to
	URL string `yaml:"url"`
	// Maximum time one delivery attempt may take, including any token request it triggers
	Timeout time.Duration `yaml:"timeout"`
	// How the requests authenticate
	Auth ConfigOutboundAuth `yaml:"auth"`
}

// ConfigOutboundAuth configures how a request this service sends authenticates
type ConfigOutboundAuth struct {
	// How the request authenticates: `hmac` and `ed25519` sign it following the Standard Webhooks specification, `oauth2` sends a client-credentials bearer token, and `none` sends nothing
	Type string `yaml:"type"`

	// HMAC signing, used when `type` is `hmac`
	HMAC ConfigOutboundHMAC `yaml:"hmac"`

	// OAuth2 client credentials, used when `type` is `oauth2`
	OAuth2 ConfigOutboundOAuth2 `yaml:"oauth2"`

	// Must be true for `type` to be `none`, as an explicit acknowledgement that the receiver can't tell these requests from forged ones
	AllowUnauthenticated bool `yaml:"allowUnauthenticated"`
}

// ConfigOutboundHMAC configures Standard Webhooks HMAC signing
type ConfigOutboundHMAC struct {
	// Secrets the requests are signed with, in the Standard Webhooks `whsec_` form
	// Every secret signs, so the receiver can rotate by adding the new secret here before removing the old one
	Secrets []string `yaml:"secrets"`
}

// ConfigOutboundOAuth2 configures an OAuth2 client-credentials grant
type ConfigOutboundOAuth2 struct {
	// Token endpoint of the identity provider
	TokenURL string `yaml:"tokenUrl"`
	// Client ID of this service at the provider
	ClientID string `yaml:"clientId"`
	// Client secret, as an alternative to `clientAssertion`
	ClientSecret string `yaml:"clientSecret"`
	// Federated credential to authenticate with instead of a client secret, per RFC 7523 section 2.2
	// Supported values: `AzureManagedIdentity`, `AzureManagedIdentity=<client-id>`, `AzureWorkloadIdentity`, `KubernetesServiceAccountToken`, `KubernetesServiceAccountToken=<path>`, `tsiam=<endpoint>`
	ClientAssertion string `yaml:"clientAssertion"`
	// Resource requested from tsiam, required when `clientAssertion` uses tsiam
	ClientAssertionAudience string `yaml:"clientAssertionAudience"`
	// Scopes requested for the token
	Scopes []string `yaml:"scopes"`
	// Extra parameters sent to the token endpoint, such as `audience` for providers that need one
	EndpointParams map[string]string `yaml:"endpointParams"`
}

// ConfigSigning configures the keys this service signs requests with
type ConfigSigning struct {
	// Ed25519 keys used by endpoints whose `auth.type` is `ed25519`
	Ed25519 ConfigSigningEd25519 `yaml:"ed25519"`
}

// ConfigSigningEd25519 configures the Ed25519 signing keys
//
// Every key is published at `/.well-known/provino-keys.json`, and requests are signed with the active one
type ConfigSigningEd25519 struct {
	// ID of the key requests are signed with, which must have a private key
	ActiveKey string `yaml:"activeKey"`

	// Signing keys
	// Each one has an `id` and either a private key (`privateKey`, or `privateKeyFile` for a path) or, for a retired key that is still published, a public key (`publicKey` or `publicKeyFile`)
	// Keys are PEM-encoded, or in the Standard Webhooks `whsk_` and `whpk_` forms
	// +example [{"id": "2026-10", "privateKeyFile": "/etc/provino/webhook-2026-10.pem"}]
	Keys []ConfigSigningKey `yaml:"keys"`
}

// ConfigSigningKey is one Ed25519 key
type ConfigSigningKey struct {
	// ID of the key, published as its `kid`
	ID string `yaml:"id"`
	// Private key, PEM-encoded or in the `whsk_` form
	PrivateKey string `yaml:"privateKey"`
	// Path of a file holding the private key
	PrivateKeyFile string `yaml:"privateKeyFile"`
	// Public key of a retired key, PEM-encoded or in the `whpk_` form
	PublicKey string `yaml:"publicKey"`
	// Path of a file holding the public key of a retired key
	PublicKeyFile string `yaml:"publicKeyFile"`
}

// ConfigImages represents the limits for images and variants
type ConfigImages struct {
	// Maximum size of an uploaded image, in bytes
	// +default 33554432 (32 MB)
	MaxUploadSize int64 `yaml:"maxUploadSize"`

	// Maximum number of pixels in an uploaded image, after its EXIF orientation is applied
	// It bounds the memory decoding an image can take, whatever its size in bytes
	// +default 100_000_000
	MaxSourcePixels int64 `yaml:"maxSourcePixels"`

	// Maximum number of variants a single job can ask for
	// A request names every variant an image needs, including the ones it already has
	// +default 48
	MaxVariants int `yaml:"maxVariants"`

	// Maximum width or height, in pixels, of a requested variant
	// +default 8192
	MaxResolution int `yaml:"maxResolution"`

	// Maximum number of jobs that can wait for one image while another of its jobs runs
	// +default 10
	MaxQueuedJobs int `yaml:"maxQueuedJobs"`

	// Maximum number of encoding tasks Francis runs on each host at once.
	// Encoding is CPU- and memory-bound, so this bounds the resources a burst of jobs can consume while allowing excess tasks to move to another host.
	// Variants are encoded one per thread, so this is the only concurrency knob: libvips itself is configured to use a single thread per operation.
	// If empty, defaults to the number of CPUs the process can use, which follows the CPU limit of a container.
	MaxConcurrentEncodes int `yaml:"maxConcurrentEncodes"`

	// Maximum amount of memory, in bytes, libvips uses for its operation cache
	// +default 52428800 (50MB)
	VipsCacheMaxMemory int `yaml:"vipsCacheMaxMemory"`
}

// ConfigStorage configures where the source images and the generated variants are stored
type ConfigStorage struct {
	// Where the uploaded source images are stored
	// Local file system: `fs:/path/to/folder`, `local:/path/to/folder`, or a path with no protocol at all
	// S3: `s3://<access-key>:<secret-key>@s3.<region>.amazonaws.com/<bucket>?prefix=<key-prefix>&sessionToken=<token>`
	// Vercel Blob with a read-write token: `vercelblob:?tokenEnv=<environment-variable>&access=private&prefix=<key-prefix>`
	// Vercel Blob with OIDC: `vercelblob:?auth=oidc&storeIdEnv=<environment-variable>&access=private&prefix=<key-prefix>`
	// +default "./data/originals"
	Originals string `yaml:"originals"`

	// Where the generated variants and the manifests are stored, using the same connection strings as `originals`
	// +default "./data/variants"
	Variants string `yaml:"variants"`

	// Folder on the local file system the source images are cached in while their variants are generated
	// Every variant of an image is encoded from the same source, so each job downloads it once into this folder and every step that follows reads it from there
	// A job deletes its file when it terminates, and the ones a host left behind by stopping in the middle of a job are swept once they are older than twice `actors.workflowTimeout`
	// Unlike `originals` and `variants` this is a path rather than a connection string, because it is never anything but a local folder
	// It holds only files that can be downloaded again, so it belongs on ephemeral storage: in a container that is a writable `/tmp`
	// +default a folder inside the system temporary directory
	SourceCache string `yaml:"sourceCache"`
}

// ConfigRateLimit configures the Francis rate-limit actor protecting browser uploads
type ConfigRateLimit struct {
	// Number of uploads admitted per caller during each `per` interval
	// +default 2
	Rate int `yaml:"rate"`

	// Interval over which `rate` uploads are admitted
	// +default "1m"
	Per time.Duration `yaml:"per"`

	// Maximum number of uploads from one caller that can be admitted at once before throttling starts
	// +default 10
	Burst int `yaml:"burst"`
}

// ConfigActors represents the configuration of the Francis actor host that executes the jobs
type ConfigActors struct {
	// How the actor runtime this service uses is provided. Supported values: `embedded` (default), `external`
	// With `embedded` the runtime runs inside this process and persists to `actors.database`, so there's nothing else to deploy
	// With `external` this process is only an actor host, and it connects to a standalone Francis runtime configured under `actors.external`, which owns the database instead
	// +default "embedded"
	Runtime string `yaml:"runtime"`

	// Address (`host:port`) at which this actor host can be reached by its peers
	// In a single-instance deployment this only needs to be an address the process itself can bind to
	// The `PROVINO_ACTORS_ADDRESS` environment variable overrides it, which is how instances that share one configuration file each advertise their own address
	// +default "127.0.0.1:7571"
	Address string `yaml:"address"`

	// Address to bind the actor host's peer server to
	// If empty, the host part of `address` is used
	BindAddress string `yaml:"bindAddress"`

	// Port to bind the actor host's peer server to
	// If empty, the port part of `address` is used
	BindPort int `yaml:"bindPort"`

	// Database the embedded actor runtime persists actor state to, as a connection string
	// SQLite: `sqlite://<path-to-file>`, or a path with no protocol at all
	// Postgres: `postgres://<user>:<password>@<host>:<port>/<database>`, optionally with connection options (`postgresql://` is an alias). Required to run more than one instance against the same jobs
	// Ignored when `actors.runtime` is `external`, since the standalone runtime owns the database in that case
	// +default "sqlite://./data/actors.db"
	Database string `yaml:"database"`

	// Pre-shared key the cluster CA is derived from: actor hosts that share the same key authenticate each other with mTLS.
	// Recommended to generate with `openssl rand -base64 32`
	// If empty, a random key is generated at startup, which is only usable when running a single instance
	// Only used when `actors.runtime` is `embedded`: with an external runtime the cluster CA comes from the runtime itself
	RuntimePSK string `yaml:"runtimePSK"` //nolint:tagliatelle

	// Configuration for connecting to a standalone Francis runtime, used when `actors.runtime` is `external`
	External ConfigActorsExternal `yaml:"external"`

	// How long each workflow handler attempt and compensation may execute after a worker starts it
	// Queue wait and retry backoff do not consume this execution budget
	// The complete workflow has a separate backstop of six times this value, which starts when the job leaves the image's queue
	// +default "10m"
	WorkflowTimeout time.Duration `yaml:"workflowTimeout"`

	// How long the status of a finished job is retained before it expires.
	// Set to 0 to retain it indefinitely.
	// +default "24h"
	CompletedWorkflowRetention time.Duration `yaml:"completedWorkflowRetention"`
}

// ConfigActorsExternal represents the configuration for connecting to a standalone Francis runtime
type ConfigActorsExternal struct {
	// Addresses (`host:port`) of the runtime replicas this host connects to.
	// The host connects to one at a time and rolls over to another when it fails.
	// Required when `actors.runtime` is `external`.
	Addresses []string `yaml:"addresses"`

	// Pre-shared key this host authenticates to the runtime with.
	// Exactly one of `bootstrapPSK` and `bootstrapTokenFile` is required when `actors.runtime` is `external`.
	BootstrapPSK string `yaml:"bootstrapPSK"` //nolint:tagliatelle

	// Path of a file containing the JWT this host authenticates to the runtime with.
	// The file is re-read on every bootstrap, so a rotated token (such as a Kubernetes projected service account token) is picked up.
	BootstrapTokenFile string `yaml:"bootstrapTokenFile"`

	// Path of a file containing the PEM-encoded cluster CA certificates this host trusts before its first connection to the runtime.
	// With the CA pinned, the host can verify the runtime from the very first connection.
	CAPath string `yaml:"caPath"`

	// PEM-encoded cluster CA certificates, as an alternative to `caPath`.
	CAPEM string `yaml:"caPEM"` //nolint:tagliatelle

	// If true, the host trusts the runtime's certificate on the first connection instead of pinning the cluster CA.
	// This is unsafe: anyone able to intercept that first connection can impersonate the runtime, which also exposes the bootstrap token when authenticating with a JWT.
	// Exactly one of the CA options and this one must be set when `actors.runtime` is `external`.
	UnsafeNoPinnedCA bool `yaml:"unsafeNoPinnedCA"` //nolint:tagliatelle
}

// ConfigLogs represents logging configuration
type ConfigLogs struct {
	// Controls log level and verbosity. Supported values: `debug`, `info` (default), `warn`, `error`
	// +default "info"
	Level string `yaml:"level"`

	// If true, calls to the healthcheck endpoint (`/healthz`) are not included in the logs
	// +default true
	OmitHealthChecks bool `yaml:"omitHealthChecks"`

	// If true, emits logs formatted as JSON, otherwise uses a text-based structured log format.
	// Defaults to false if a TTY is attached (e.g. when running the binary directly in the terminal or in development); true otherwise.
	JSON bool `yaml:"json"`
}

// ConfigDev includes options using during development only
type ConfigDev struct {
}

// Internal properties
type internal struct {
	instanceID       string
	configFileLoaded string   // Path to the config file that was loaded
	runtimePSK       []byte   // Runtime PSK, either from the config or randomly generated during Validate
	database         Database // Database the embedded actor runtime persists to, parsed from the DSN during Validate
	externalCAPEM    []byte   // PEM-encoded cluster CA the host pins, read from the config or from disk during Validate
}

// String implements fmt.Stringer and prints out the config for debugging
func (c *Config) String() string {
	//nolint:errchkjson,musttag
	enc, _ := json.Marshal(c)
	return string(enc)
}

// GetLoadedConfigPath returns the path to the config file that was loaded
func (c *Config) GetLoadedConfigPath() string {
	return c.internal.configFileLoaded
}

// SetLoadedConfigPath sets the path to the config file that was loaded
func (c *Config) SetLoadedConfigPath(filePath string) {
	c.internal.configFileLoaded = filePath
}

// GetInstanceID returns the instance ID
func (c *Config) GetInstanceID() string {
	return c.internal.instanceID
}

// GetRuntimePSK returns the pre-shared key the embedded actor runtime derives its cluster CA from
// It's either the operator-provided `actors.runtimePSK` or the random one generated during Validate
func (c *Config) GetRuntimePSK() []byte {
	return c.internal.runtimePSK
}

// GetDatabase returns the database the embedded actor runtime persists to, parsed from `actors.database` during Validate
func (c *Config) GetDatabase() Database {
	return c.internal.database
}

// GetExternalCAPEM returns the PEM-encoded cluster CA the host pins when connecting to an external runtime
// It's empty when the operator opted out of pinning with `actors.external.unsafeNoPinnedCA`
func (c *Config) GetExternalCAPEM() []byte {
	return c.internal.externalCAPEM
}

// Validate the configuration and performs some sanitization
func (c *Config) Validate(logger *slog.Logger) error {
	err := c.validateServer()
	if err != nil {
		return err
	}

	err = c.validateAuth()
	if err != nil {
		return err
	}

	err = c.validateActors(logger)
	if err != nil {
		return err
	}

	err = c.validateRateLimit()
	if err != nil {
		return err
	}

	err = c.validateStorage()
	if err != nil {
		return err
	}

	err = c.validateImages()
	if err != nil {
		return err
	}

	// Endpoints come before the authorizer, which can name one, and after the images, whose limits the static authorizer's variants are checked against
	err = c.validateSigning()
	if err != nil {
		return err
	}

	err = c.validateNotify()
	if err != nil {
		return err
	}

	err = c.validateAuthorizer()
	if err != nil {
		return err
	}

	return nil
}

// Validate the "server" section
func (c *Config) validateServer() error {
	if c.Server.Port < 0 {
		return errors.New("configuration option 'server.port' must not be negative")
	}

	c.Server.PublicURL = strings.TrimRight(strings.TrimSpace(c.Server.PublicURL), "/")
	if !isHTTPOrigin(c.Server.PublicURL) {
		return errors.New("configuration option 'server.publicUrl' must be an absolute HTTP origin")
	}

	// An origin is compared verbatim against the Origin header, so it is normalized the way browsers send it
	for i, origin := range c.Server.CORSAllowedOrigins {
		origin = strings.TrimRight(strings.TrimSpace(origin), "/")
		if !isHTTPOrigin(origin) {
			return fmt.Errorf("configuration option 'server.corsAllowedOrigins' contains '%s', which is not an absolute HTTP origin", c.Server.CORSAllowedOrigins[i])
		}
		c.Server.CORSAllowedOrigins[i] = origin
	}

	// Validate proxy trust before forwarded addresses can become rate-limit keys
	for _, trustedProxy := range c.Server.TrustedProxies {
		if trustedProxy == "" {
			return errors.New("configuration option 'server.trustedProxies' must not contain empty entries")
		}

		_, _, cidrErr := net.ParseCIDR(trustedProxy)
		ip := net.ParseIP(trustedProxy)
		if cidrErr != nil && ip == nil {
			return errors.New("configuration option 'server.trustedProxies' entries must be IPs or CIDRs")
		}
	}

	if c.Server.ReadTimeout <= 0 {
		return errors.New("configuration option 'server.readTimeout' must be greater than zero")
	}
	if c.Server.WriteTimeout <= 0 {
		return errors.New("configuration option 'server.writeTimeout' must be greater than zero")
	}

	return nil
}

// Validate the "auth" section
func (c *Config) validateAuth() error {
	jwtEnabled := c.Auth.JWT.Issuer != ""
	apiKeysEnabled := len(c.Auth.APIKeys) > 0

	// Disabling authentication is a choice that can't coexist with credentials, since a request without them would otherwise be accepted anyway
	if c.Auth.None.Enabled {
		if jwtEnabled || apiKeysEnabled {
			return errors.New("configuration option 'auth.none.enabled' cannot be combined with 'auth.jwt' or 'auth.apiKeys'")
		}
		if strings.TrimSpace(c.Auth.None.Subject) == "" {
			return errors.New("configuration option 'auth.none.subject' is required when 'auth.none.enabled' is set")
		}
		return nil
	}
	if !jwtEnabled && !apiKeysEnabled {
		return errors.New("no authentication is configured: set 'auth.jwt.issuer', add 'auth.apiKeys', or set 'auth.none.enabled' for development")
	}

	if jwtEnabled {
		err := c.validateJWT()
		if err != nil {
			return err
		}
	}

	err := c.validateAPIKeys()
	if err != nil {
		return err
	}

	return nil
}

// Validate the JWT verifier's settings, which are used at startup to fetch the key set
func (c *Config) validateJWT() error {
	jwt := &c.Auth.JWT

	issuer, err := url.Parse(strings.TrimSpace(jwt.Issuer))
	if err != nil || !issuer.IsAbs() || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("configuration option 'auth.jwt.issuer' must be an absolute HTTP URL")
	}
	jwt.Issuer = strings.TrimSpace(jwt.Issuer)

	if jwt.JWKSURL != "" {
		jwksURL, err := url.Parse(jwt.JWKSURL)
		if err != nil || !jwksURL.IsAbs() || (jwksURL.Scheme != "https" && jwksURL.Scheme != "http") || jwksURL.Host == "" || jwksURL.User != nil {
			return errors.New("configuration option 'auth.jwt.jwksUrl' must be an absolute HTTP URL")
		}
	}

	// The audience defaults to this service's own origin, which is what a provider identifier usually is
	if jwt.Audience == "" {
		jwt.Audience = c.Server.PublicURL
	}
	if jwt.SubjectClaim == "" {
		jwt.SubjectClaim = "sub"
	}
	if jwt.ScopeClaim == "" {
		jwt.ScopeClaim = "scope"
	}

	return nil
}

// Validate the API keys, normalizing every one to the digest it is looked up by
func (c *Config) validateAPIKeys() error {
	ids := make(map[string]struct{}, len(c.Auth.APIKeys))
	digests := make(map[string]struct{}, len(c.Auth.APIKeys))
	for i := range c.Auth.APIKeys {
		key := &c.Auth.APIKeys[i]
		if !nameRegex.MatchString(key.ID) {
			return fmt.Errorf("configuration option 'auth.apiKeys' entry %d needs an 'id' of up to 64 letters, digits, dots, underscores or dashes", i)
		}

		_, duplicate := ids[key.ID]
		if duplicate {
			return fmt.Errorf("configuration option 'auth.apiKeys' contains the id '%s' more than once", key.ID)
		}
		ids[key.ID] = struct{}{}

		// Exactly one form of the key, and the plain one has the prefix that routes it away from the JWT verifier
		switch {
		case key.Key != "" && key.SHA256 != "":
			return fmt.Errorf("configuration option 'auth.apiKeys' entry '%s' sets both 'key' and 'sha256'", key.ID)
		case key.Key != "":
			if !strings.HasPrefix(key.Key, APIKeyPrefix) || len(key.Key) < len(APIKeyPrefix)+24 {
				return fmt.Errorf("configuration option 'auth.apiKeys' entry '%s' has a 'key' that doesn't start with '%s' followed by at least 24 characters", key.ID, APIKeyPrefix)
			}
		case key.SHA256 != "":
			key.SHA256 = strings.ToLower(strings.TrimSpace(key.SHA256))
			decoded, err := hex.DecodeString(key.SHA256)
			if err != nil || len(decoded) != 32 {
				return fmt.Errorf("configuration option 'auth.apiKeys' entry '%s' has a 'sha256' that isn't a hex-encoded SHA-256 digest", key.ID)
			}
		default:
			return fmt.Errorf("configuration option 'auth.apiKeys' entry '%s' needs a 'key' or a 'sha256'", key.ID)
		}

		digest := key.Digest()
		_, duplicate = digests[digest]
		if duplicate {
			return fmt.Errorf("configuration option 'auth.apiKeys' entry '%s' repeats the key of another entry", key.ID)
		}
		digests[digest] = struct{}{}

		if key.Subject == "" {
			key.Subject = key.ID
		}
	}

	return nil
}

// Validate the "signing" section's structure, while the keys themselves are parsed when the signer is created at startup
func (c *Config) validateSigning() error {
	ed := c.Signing.Ed25519
	if len(ed.Keys) == 0 {
		if ed.ActiveKey != "" {
			return errors.New("configuration option 'signing.ed25519.activeKey' is set but there are no keys in 'signing.ed25519.keys'")
		}
		return nil
	}

	ids := make(map[string]bool, len(ed.Keys))
	for i, key := range ed.Keys {
		if !nameRegex.MatchString(key.ID) {
			return fmt.Errorf("configuration option 'signing.ed25519.keys' entry %d needs an 'id' of up to 64 letters, digits, dots, underscores or dashes", i)
		}
		_, found := ids[key.ID]
		if found {
			return fmt.Errorf("configuration option 'signing.ed25519.keys' contains the id '%s' more than once", key.ID)
		}

		sources := 0
		for _, value := range []string{key.PrivateKey, key.PrivateKeyFile, key.PublicKey, key.PublicKeyFile} {
			if value != "" {
				sources++
			}
		}
		if sources != 1 {
			return fmt.Errorf("configuration option 'signing.ed25519.keys' entry '%s' needs exactly one of 'privateKey', 'privateKeyFile', 'publicKey', and 'publicKeyFile'", key.ID)
		}

		ids[key.ID] = key.PrivateKey != "" || key.PrivateKeyFile != ""
	}

	hasPrivate, found := ids[ed.ActiveKey]
	switch {
	case ed.ActiveKey == "":
		return errors.New("configuration option 'signing.ed25519.activeKey' is required when keys are configured")
	case !found:
		return fmt.Errorf("configuration option 'signing.ed25519.activeKey' names '%s', which is not in 'signing.ed25519.keys'", ed.ActiveKey)
	case !hasPrivate:
		return fmt.Errorf("configuration option 'signing.ed25519.activeKey' names '%s', which has no private key", ed.ActiveKey)
	}

	return nil
}

// Validate the "notify" section
func (c *Config) validateNotify() error {
	for name, endpoint := range c.Notify.Endpoints {
		if !nameRegex.MatchString(name) {
			return fmt.Errorf("configuration option 'notify.endpoints' contains '%s', which is not a valid name: use up to 64 letters, digits, dots, underscores or dashes", name)
		}
		option := "notify.endpoints." + name

		if !isHTTPURL(endpoint.URL) {
			return fmt.Errorf("configuration option '%s.url' must be an absolute HTTP URL", option)
		}
		if endpoint.Timeout < 0 {
			return fmt.Errorf("configuration option '%s.timeout' must not be negative", option)
		}
		if endpoint.Timeout == 0 {
			endpoint.Timeout = 15 * time.Second
		}

		err := c.validateOutboundAuth(&endpoint.Auth, option+".auth")
		if err != nil {
			return err
		}

		c.Notify.Endpoints[name] = endpoint
	}

	// A single endpoint is the obvious default, which spares requests from naming it
	if c.Notify.DefaultEndpoint == "" && len(c.Notify.Endpoints) == 1 {
		for name := range c.Notify.Endpoints {
			c.Notify.DefaultEndpoint = name
		}
	}
	if c.Notify.DefaultEndpoint != "" {
		_, found := c.Notify.Endpoints[c.Notify.DefaultEndpoint]
		if !found {
			return fmt.Errorf("configuration option 'notify.defaultEndpoint' names '%s', which is not in 'notify.endpoints'", c.Notify.DefaultEndpoint)
		}
	}

	return nil
}

// Validate how one outbound request authenticates
func (c *Config) validateOutboundAuth(auth *ConfigOutboundAuth, option string) error {
	switch auth.Type {
	case OutboundAuthHMAC:
		if len(auth.HMAC.Secrets) == 0 {
			return fmt.Errorf("configuration option '%s.hmac.secrets' needs at least one secret when '%s.type' is 'hmac'", option, option)
		}
		for i, secret := range auth.HMAC.Secrets {
			if !strings.HasPrefix(secret, "whsec_") {
				return fmt.Errorf("configuration option '%s.hmac.secrets' entry %d must be in the Standard Webhooks form, starting with 'whsec_'", option, i)
			}
		}

	case OutboundAuthEd25519:
		if len(c.Signing.Ed25519.Keys) == 0 {
			return fmt.Errorf("configuration option '%s.type' is 'ed25519', but there are no keys in 'signing.ed25519.keys'", option)
		}

	case OutboundAuthOAuth2:
		oauth2 := &auth.OAuth2
		if !isHTTPURL(oauth2.TokenURL) {
			return fmt.Errorf("configuration option '%s.oauth2.tokenUrl' must be an absolute HTTP URL", option)
		}
		if oauth2.ClientID == "" {
			return fmt.Errorf("configuration option '%s.oauth2.clientId' is required when '%s.type' is 'oauth2'", option, option)
		}
		if (oauth2.ClientSecret == "") == (oauth2.ClientAssertion == "") {
			return fmt.Errorf("exactly one of the configuration options '%s.oauth2.clientSecret' and '%s.oauth2.clientAssertion' is required", option, option)
		}

	case OutboundAuthNone:
		if !auth.AllowUnauthenticated {
			return fmt.Errorf("configuration option '%s.type' is 'none', which also requires '%s.allowUnauthenticated' to be true", option, option)
		}

	case "":
		return fmt.Errorf("configuration option '%s.type' is required: one of '%s', '%s', '%s', '%s'", option, OutboundAuthHMAC, OutboundAuthEd25519, OutboundAuthOAuth2, OutboundAuthNone)

	default:
		return fmt.Errorf("configuration option '%s.type' is invalid: must be one of '%s', '%s', '%s', '%s'", option, OutboundAuthHMAC, OutboundAuthEd25519, OutboundAuthOAuth2, OutboundAuthNone)
	}

	return nil
}

// Validate the "authorizer" section
func (c *Config) validateAuthorizer() error {
	switch c.Authorizer.Type {
	case "":
		return nil

	case AuthorizerHTTP:
		if !isHTTPURL(c.Authorizer.HTTP.URL) {
			return errors.New("configuration option 'authorizer.http.url' must be an absolute HTTP URL")
		}
		if c.Authorizer.HTTP.Timeout < 0 {
			return errors.New("configuration option 'authorizer.http.timeout' must not be negative")
		}
		if c.Authorizer.HTTP.Timeout == 0 {
			c.Authorizer.HTTP.Timeout = 10 * time.Second
		}
		return c.validateOutboundAuth(&c.Authorizer.HTTP.Auth, "authorizer.http.auth")

	case AuthorizerStatic:
		static := &c.Authorizer.Static
		err := imagekeys.ValidateNamespace(static.Namespace)
		if err != nil {
			return fmt.Errorf("configuration option 'authorizer.static.namespace' is invalid: %w", err)
		}
		if static.MaxBytes < 0 {
			return errors.New("configuration option 'authorizer.static.maxBytes' must not be negative")
		}
		for _, format := range static.AllowedFormats {
			if !slices.Contains(SourceFormats, strings.ToLower(format)) {
				return fmt.Errorf("configuration option 'authorizer.static.allowedFormats' contains '%s', which is not one of %s", format, strings.Join(SourceFormats, ", "))
			}
		}
		static.Variants, err = variant.NormalizeSet(static.Variants, c.Images.MaxResolution, c.Images.MaxVariants)
		if err != nil {
			return fmt.Errorf("configuration option 'authorizer.static.variants' is invalid: %w", err)
		}
		if static.Notify != nil {
			return c.validateNotifyTarget(static.Notify.Endpoint, "authorizer.static.notify.endpoint")
		}
		return nil

	default:
		return fmt.Errorf("configuration option 'authorizer.type' is invalid: must be empty or one of '%s', '%s'", AuthorizerHTTP, AuthorizerStatic)
	}
}

// Check that a notification can be delivered to the endpoint it names, or to the default one when it names none
func (c *Config) validateNotifyTarget(endpoint string, option string) error {
	if endpoint == "" {
		if c.Notify.DefaultEndpoint == "" {
			return fmt.Errorf("configuration option '%s' is empty and there is no default endpoint in 'notify'", option)
		}
		return nil
	}

	_, found := c.Notify.Endpoints[endpoint]
	if !found {
		return fmt.Errorf("configuration option '%s' names '%s', which is not in 'notify.endpoints'", option, endpoint)
	}
	return nil
}

// Validate the rate limiter before its built-in actor is constructed
func (c *Config) validateRateLimit() error {
	// Francis needs a positive refill rate and bucket capacity
	if c.RateLimit.Rate <= 0 {
		return errors.New("configuration option 'rateLimit.rate' must be greater than zero")
	}
	if c.RateLimit.Per <= 0 {
		return errors.New("configuration option 'rateLimit.per' must be greater than zero")
	}
	if c.RateLimit.Burst <= 0 {
		return errors.New("configuration option 'rateLimit.burst' must be greater than zero")
	}

	return nil
}

// Validate the "actors" section and resolve the values the actor host needs at startup
func (c *Config) validateActors(logger *slog.Logger) error {
	envAddress := os.Getenv(EnvActorsAddress)
	if envAddress != "" {
		c.Actors.Address = envAddress
	}

	// The address is what peers connect to, so it must contain a usable port even in single-host deployments
	addrHost, addrPort, err := net.SplitHostPort(c.Actors.Address)
	if err != nil {
		return fmt.Errorf("configuration option 'actors.address' is invalid: must be in the format 'host:port': %w", err)
	}
	port, err := strconv.Atoi(addrPort)
	if err != nil || port <= 0 {
		return errors.New("configuration option 'actors.address' is invalid: port is not valid")
	}
	if c.Actors.BindAddress == "" {
		c.Actors.BindAddress = addrHost
	}
	if c.Actors.BindPort <= 0 {
		c.Actors.BindPort = port
	}

	if c.Actors.WorkflowTimeout <= 0 {
		return errors.New("configuration option 'actors.workflowTimeout' must be greater than zero")
	}
	if c.Actors.CompletedWorkflowRetention < 0 {
		return errors.New("configuration option 'actors.completedWorkflowRetention' must not be negative")
	}

	// The two topologies need entirely different things, so only the options of the selected one are validated
	switch c.Actors.Runtime {
	case ActorRuntimeEmbedded:
		return c.validateEmbeddedRuntime(logger)
	case ActorRuntimeExternal:
		return c.validateExternalRuntime()
	default:
		return fmt.Errorf("configuration option 'actors.runtime' is invalid: must be one of '%s', '%s'", ActorRuntimeEmbedded, ActorRuntimeExternal)
	}
}

// Validate the options the embedded runtime needs: its own database, and the key its cluster CA is derived from
func (c *Config) validateEmbeddedRuntime(logger *slog.Logger) error {
	database, err := ParseDatabase(c.Actors.Database)
	if err != nil {
		return fmt.Errorf("configuration option 'actors.database' is invalid: %w", err)
	}
	c.internal.database = database

	// The PSK only guards host-to-host mTLS, so a process-lifetime key is enough for a single instance
	// A restart invalidates it, which is harmless as long as no other host needs to authenticate against this one
	return c.resolveRuntimePSK(logger)
}

// Validate the options needed to connect to a standalone runtime
// Francis requires both an explicit bootstrap method and an explicit trust decision, so both are checked here rather than left to fail at startup
func (c *Config) validateExternalRuntime() error {
	if len(c.Actors.External.Addresses) == 0 {
		return errors.New("configuration option 'actors.external.addresses' is required when 'actors.runtime' is 'external'")
	}
	if slices.Contains(c.Actors.External.Addresses, "") {
		return errors.New("configuration option 'actors.external.addresses' must not contain empty entries")
	}

	// The runtime authenticates the host with exactly one of the two methods, so configuring both is a mistake rather than a fallback
	hasPSK := c.Actors.External.BootstrapPSK != ""
	hasToken := c.Actors.External.BootstrapTokenFile != ""
	switch {
	case hasPSK && hasToken:
		return errors.New("configuration options 'actors.external.bootstrapPSK' and 'actors.external.bootstrapTokenFile' are mutually exclusive")
	case !hasPSK && !hasToken:
		return errors.New("one of the configuration options 'actors.external.bootstrapPSK' and 'actors.external.bootstrapTokenFile' is required when 'actors.runtime' is 'external'")
	}

	return c.resolveExternalCA()
}

// Resolve the cluster CA the host pins, which is what lets it verify the runtime on its very first connection
func (c *Config) resolveExternalCA() error {
	external := c.Actors.External

	hasCA := external.CAPath != "" || external.CAPEM != ""
	switch {
	case hasCA && external.UnsafeNoPinnedCA:
		return errors.New("configuration option 'actors.external.unsafeNoPinnedCA' cannot be set together with 'actors.external.caPath' or 'actors.external.caPEM'")
	case !hasCA && !external.UnsafeNoPinnedCA:
		return errors.New("one of the configuration options 'actors.external.caPath' and 'actors.external.caPEM' is required when 'actors.runtime' is 'external', unless 'actors.external.unsafeNoPinnedCA' is set")
	case external.UnsafeNoPinnedCA:
		return nil
	}

	if external.CAPEM != "" {
		c.internal.externalCAPEM = []byte(external.CAPEM)
		return nil
	}

	// The file is read once, at startup, because a host that can't verify the runtime has nothing to fall back on anyway
	caPEM, err := os.ReadFile(external.CAPath)
	if err != nil {
		return fmt.Errorf("failed to read the cluster CA from the file in configuration option 'actors.external.caPath': %w", err)
	}
	if len(caPEM) == 0 {
		return errors.New("the file in configuration option 'actors.external.caPath' is empty")
	}
	c.internal.externalCAPEM = caPEM

	return nil
}

// Resolve the runtime PSK from the configuration, generating a random one when the operator didn't set any
func (c *Config) resolveRuntimePSK(logger *slog.Logger) error {
	if c.Actors.RuntimePSK != "" {
		c.internal.runtimePSK = []byte(c.Actors.RuntimePSK)
		return nil
	}

	buf := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, buf)
	if err != nil {
		return fmt.Errorf("failed to generate random runtime PSK: %w", err)
	}
	c.internal.runtimePSK = buf

	logger.Debug("No 'actors.runtimePSK' set; a random key will be generated for this process lifetime")

	return nil
}

// Validate the "storage" section
func (c *Config) validateStorage() error {
	if c.Storage.Originals == "" {
		return errors.New("configuration option 'storage.originals' is required")
	}
	if c.Storage.Variants == "" {
		return errors.New("configuration option 'storage.variants' is required")
	}

	// Source images and variants are written and cleaned up independently, so sharing a location would let one overwrite the other's objects
	if c.Storage.Originals == c.Storage.Variants {
		return errors.New("configuration options 'storage.originals' and 'storage.variants' must not point to the same location")
	}

	// The cache holds local files rather than objects, so its default is the folder a container is expected to have writable space in rather than one of the DSNs above
	// It is resolved here because the system temporary directory is only known at runtime
	c.Storage.SourceCache = strings.TrimSpace(c.Storage.SourceCache)
	if c.Storage.SourceCache == "" {
		c.Storage.SourceCache = filepath.Join(os.TempDir(), defaultSourceCacheDirName)
	}

	return nil
}

// Validate the "images" section
func (c *Config) validateImages() error {
	if c.Images.MaxUploadSize <= 0 {
		return errors.New("configuration option 'images.maxUploadSize' must be greater than zero")
	}
	if c.Images.MaxSourcePixels <= 0 {
		return errors.New("configuration option 'images.maxSourcePixels' must be greater than zero")
	}
	if c.Images.MaxVariants <= 0 {
		return errors.New("configuration option 'images.maxVariants' must be greater than zero")
	}
	if c.Images.MaxResolution <= 0 {
		return errors.New("configuration option 'images.maxResolution' must be greater than zero")
	}
	if c.Images.MaxQueuedJobs <= 0 {
		return errors.New("configuration option 'images.maxQueuedJobs' must be greater than zero")
	}
	if c.Images.MaxConcurrentEncodes < 0 {
		return errors.New("configuration option 'images.maxConcurrentEncodes' must not be negative")
	}
	if c.Images.VipsCacheMaxMemory < 0 {
		return errors.New("configuration option 'images.vipsCacheMaxMemory' must not be negative")
	}

	return nil
}

// Report whether a value is an absolute HTTP origin, with no path, query or credentials
func isHTTPOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.IsAbs() && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

// Report whether a value is an absolute HTTP URL without credentials
func isHTTPURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.IsAbs() && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Fragment == ""
}

// SourceFormats are the formats an uploaded image may be in, by the extension its original is stored with
var SourceFormats = sniff.Formats

// Digest returns the hex-encoded SHA-256 of the key, which is what a presented key is compared against
func (k ConfigAPIKey) Digest() string {
	if k.Key != "" {
		sum := sha256.Sum256([]byte(k.Key))
		return hex.EncodeToString(sum[:])
	}
	return k.SHA256
}
