// source.go defines the provider-neutral boundary that quota provider adapters
// (added in a later task) sit behind: the QuotaSource interface, a bounded HTTP
// client that enforces response-size, timeout, and HTTPS policy, a transient
// credential resolver, and a minimal source registry.
//
// This file introduces the package's first (and only) networking seam. The
// bounded client wraps an injectable HTTPDoer so tests never touch the network.
// Resolved credentials are transient: they are returned to the caller for the
// immediate request only and are never persisted, logged, or included in error
// messages. All errors are sanitized via the package's SanitizeError.

package quota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// SupportStatus reports whether a source is ready to make provider requests.
type SupportStatus struct {
	// Supported is true when the adapter has current contract evidence and
	// valid configuration.
	Supported bool
	// Reason is a sanitized remediation diagnostic when not supported. It is
	// empty when Supported is true.
	Reason string
}

// QuotaSource is the provider-neutral boundary for polling one provider
// mapping's quota. Implementations perform bounded HTTP requests using
// transiently resolved credentials and return a sanitized snapshot. They NEVER
// persist credentials, raw responses, or auth headers.
//
// An unsupported source must fail closed: Fetch returns an error without making
// any HTTP request, and Status reports why it is not supported.
type QuotaSource interface {
	// MappingID returns the provider mapping this source serves.
	MappingID() string
	// Fetch retrieves the current quota snapshot via a bounded HTTP request.
	Fetch(ctx context.Context) (QuotaSnapshot, error)
	// Status reports whether this source is supported and ready.
	Status() SupportStatus
}

// --- Bounded HTTP client --------------------------------------------------

// HTTPDoer is the injectable transport seam. Tests pass a fake; production
// passes an *http.Client (or http.DefaultTransport via an adapter).
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// BoundedResponse holds the bounded, in-memory response. Body is already read
// and size-capped; callers do not need to close it.
type BoundedResponse struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
}

// Sensible production defaults for the bounded client.
const (
	defaultHTTPTimeout  = 30 * time.Second
	defaultMaxBodyBytes = 1 << 20 // 1 MiB
)

// BoundedClient wraps an HTTPDoer with response-size, timeout, and URL policy
// enforcement. A nil Transport uses the default HTTP client (which wraps
// http.DefaultTransport). A zero Timeout or MaxBodyBytes falls back to a
// sensible default.
type BoundedClient struct {
	Transport    HTTPDoer
	Timeout      time.Duration
	MaxBodyBytes int64
}

func (c *BoundedClient) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultHTTPTimeout
}

func (c *BoundedClient) maxBody() int64 {
	if c.MaxBodyBytes > 0 {
		return c.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

// Do performs a bounded HTTP request. It validates the URL scheme and host,
// applies a per-request timeout, sends the request through the transport, and
// reads the response body up to MaxBodyBytes. It returns a BoundedResponse with
// the in-memory body, status code, and headers, or a sanitized error.
//
// It never returns raw URLs with embedded credentials, auth header values, or
// oversized bodies in errors.
func (c *BoundedClient) Do(req *http.Request) (*BoundedResponse, error) {
	if req == nil {
		return nil, errors.New("bounded http: nil request")
	}
	if strings.ToLower(req.URL.Scheme) != "https" {
		return nil, errors.New("bounded http: https required for provider endpoints")
	}
	if req.URL.Host == "" {
		return nil, errors.New("bounded http: request host is empty")
	}

	ctx, cancel := context.WithTimeout(req.Context(), c.timeout())
	defer cancel()
	req = req.WithContext(ctx)

	doer := c.Transport
	if doer == nil {
		doer = &http.Client{
			Transport:     http.DefaultTransport,
			CheckRedirect: rejectRedirect,
		}
	} else if client, ok := doer.(*http.Client); ok {
		clone := *client
		clone.CheckRedirect = rejectRedirect
		doer = &clone
	}

	resp, err := doer.Do(req)
	if err != nil {
		// Transport errors (dial, redirect, timeout) may echo the URL; sanitize.
		return nil, errors.New("bounded http: " + SanitizeError(err))
	}
	defer resp.Body.Close()

	maxBody := c.maxBody()
	// Read at most maxBody+1 bytes so an oversized body is detected without ever
	// materializing the full response.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, errors.New("bounded http: " + SanitizeError(err))
	}
	if int64(len(body)) > maxBody {
		return nil, fmt.Errorf("bounded http: response body exceeds %d bytes", maxBody)
	}

	return &BoundedResponse{
		StatusCode: resp.StatusCode,
		Body:       body,
		Headers:    resp.Header,
	}, nil
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("bounded http: redirects are not allowed for provider endpoints")
}

// --- Transient credential resolver ---------------------------------------

// CredentialKind enumerates supported credential reference types.
type CredentialKind string

const (
	// CredentialEnv resolves a credential from an environment variable.
	CredentialEnv CredentialKind = "env"
	// CredentialFile resolves a credential from a file, read transiently and
	// trimmed of surrounding whitespace.
	CredentialFile CredentialKind = "file"
	// CredentialLiteral resolves to the locator value itself. Rare; mainly for
	// testing.
	CredentialLiteral CredentialKind = "literal"
)

// CredentialRef identifies a credential by kind and locator.
type CredentialRef struct {
	Kind    CredentialKind
	Locator string // env var name, file path, or literal value
}

// CredentialResolver transiently resolves credential references. Resolved values
// are used for the immediate HTTP request only — never persisted or logged.
type CredentialResolver interface {
	Resolve(ref CredentialRef) (string, error)
}

// defaultResolver resolves env/file/literal references against the real
// environment and filesystem. It holds no state: resolved values are returned to
// the caller and never retained.
type defaultResolver struct{}

// DefaultCredentialResolver returns a CredentialResolver backed by the real
// environment and filesystem.
func DefaultCredentialResolver() CredentialResolver {
	return defaultResolver{}
}

// Resolve returns the transient credential value for ref. Errors are sanitized:
// they report only a generic "could not resolve credential: <kind>" summary and
// never include the resolved value, file contents, or raw locators.
func (defaultResolver) Resolve(ref CredentialRef) (string, error) {
	switch ref.Kind {
	case CredentialEnv:
		v, ok := os.LookupEnv(ref.Locator)
		if !ok || v == "" {
			return "", fmt.Errorf("could not resolve credential: %s", ref.Kind)
		}
		return v, nil
	case CredentialFile:
		b, err := os.ReadFile(ref.Locator)
		if err != nil {
			return "", fmt.Errorf("could not resolve credential: %s", ref.Kind)
		}
		return strings.TrimSpace(string(b)), nil
	case CredentialLiteral:
		return ref.Locator, nil
	default:
		return "", fmt.Errorf("could not resolve credential: unsupported kind %q", ref.Kind)
	}
}

// --- Source registry ------------------------------------------------------

// Registry holds configured QuotaSources keyed by mapping ID. It is the seam
// that quota check and doctor use to discover and iterate providers. It is not
// safe for concurrent use; the coordinator (a later task) serializes access.
type Registry struct {
	sources []QuotaSource
	index   map[string]int
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{index: make(map[string]int)}
}

// Register adds a source for its mapping ID, replacing any existing source for
// the same ID.
func (r *Registry) Register(s QuotaSource) {
	if r.index == nil {
		r.index = make(map[string]int)
	}
	id := s.MappingID()
	if i, ok := r.index[id]; ok {
		r.sources[i] = s
		return
	}
	r.index[id] = len(r.sources)
	r.sources = append(r.sources, s)
}

// Get returns the source for mappingID, or false when none is registered.
func (r *Registry) Get(mappingID string) (QuotaSource, bool) {
	i, ok := r.index[mappingID]
	if !ok {
		return nil, false
	}
	return r.sources[i], true
}

// All returns all registered sources. The returned slice is a copy; mutating it
// does not affect the registry.
func (r *Registry) All() []QuotaSource {
	out := make([]QuotaSource, len(r.sources))
	copy(out, r.sources)
	return out
}

// AdapterDefinition describes one built-in quota adapter. Keeping the name,
// source factory, and evidence factory together prevents runtime support checks,
// polling, and evidence registration from maintaining separate provider lists.
type AdapterDefinition struct {
	Name     string
	Evidence func(time.Time) Evidence
	New      func(string, *BoundedClient, CredentialResolver, float64, *EvidenceRegistry, time.Time) QuotaSource
}

var builtInAdapters = []AdapterDefinition{
	{
		Name:     codexProviderName,
		Evidence: CodexUsageEvidence,
		New: func(id string, client *BoundedClient, creds CredentialResolver, budget float64, reg *EvidenceRegistry, now time.Time) QuotaSource {
			return NewCodexSource(id, client, creds, "", reg, now)
		},
	},
	{
		Name:     zaiProviderName,
		Evidence: ZaiEvidence,
		New: func(id string, client *BoundedClient, creds CredentialResolver, budget float64, reg *EvidenceRegistry, now time.Time) QuotaSource {
			return NewZaiSource(id, client, creds, "", reg, now)
		},
	},
	{
		Name:     anthropicProviderName,
		Evidence: AnthropicEvidence,
		New: func(id string, client *BoundedClient, creds CredentialResolver, budget float64, reg *EvidenceRegistry, now time.Time) QuotaSource {
			return NewAnthropicSource(id, client, creds, budget, reg, now)
		},
	},
	{
		Name:     neuralwattProviderName,
		Evidence: NeuralwattEvidence,
		New: func(id string, client *BoundedClient, creds CredentialResolver, budget float64, reg *EvidenceRegistry, now time.Time) QuotaSource {
			return NewNeuralwattSource(id, client, creds, reg, now)
		},
	},
}

// AdapterDefinitions returns the built-in adapter definitions in stable name
// order. Callers must not mutate the returned slice or its function values.
func AdapterDefinitions() []AdapterDefinition {
	out := append([]AdapterDefinition(nil), builtInAdapters...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AdapterDefinitionFor returns the built-in adapter definition for name.
func AdapterDefinitionFor(name string) (AdapterDefinition, bool) {
	for _, definition := range builtInAdapters {
		if definition.Name == name {
			return definition, true
		}
	}
	return AdapterDefinition{}, false
}

// KnownAdapter reports whether name is a built-in adapter.
func KnownAdapter(name string) bool {
	_, ok := AdapterDefinitionFor(name)
	return ok
}

// RegisterBuiltInEvidence adds current built-in adapter evidence to reg. The
// evidence factories themselves own their review-date policy.
func RegisterBuiltInEvidence(reg *EvidenceRegistry, now time.Time) {
	for _, definition := range builtInAdapters {
		reg.Register(definition.Evidence(now))
	}
}
