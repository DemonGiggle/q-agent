package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ProviderModelMetaVersion = 1
	modelsDevCatalogURL      = "https://models.dev/api.json"
	maxModelsDevCatalogSize  = 16 << 20
	maxModelMetaCacheSize    = 4 << 20
)

// ProviderModelOptions contains the selector controls Models.dev describes
// for one model. Only effort and toggle controls are currently understood.
type ProviderModelOptions struct {
	Effort []string `json:"effort,omitempty"`
	Toggle bool     `json:"toggle,omitempty"`
}

// ProviderModelMeta is a versioned snapshot of one Models.dev provider's
// reasoning options. Request formats stay local to qcode's provider adapters.
type ProviderModelMeta struct {
	Version   int                             `json:"version"`
	Provider  string                          `json:"provider"`
	Source    string                          `json:"source"`
	FetchedAt string                          `json:"fetched_at,omitempty"`
	Models    map[string]ProviderModelOptions `json:"models"`
}

// EmptyProviderModelMeta returns an explicit empty snapshot. Passing it to a
// provider suppresses built-in choices while retaining local wire adapters.
func EmptyProviderModelMeta(provider string) *ProviderModelMeta {
	return &ProviderModelMeta{
		Version:  ProviderModelMetaVersion,
		Provider: provider,
		Source:   modelsDevCatalogURL,
		Models:   make(map[string]ProviderModelOptions),
	}
}

// ProviderModelMetaCachePath returns the per-user versioned metadata cache
// location for a Models.dev provider.
func ProviderModelMetaCachePath(provider string) (string, error) {
	if !supportedModelMetaProvider(provider) {
		return "", fmt.Errorf("unsupported Models.dev provider %q", provider)
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(dir, "q-agent", provider+"-model-meta-v1.json"), nil
}

// ParseProviderModelMeta extracts supported effort and toggle choices from
// one provider in the Models.dev catalog. Unsupported option types are ignored.
func ParseProviderModelMeta(data []byte, providerID string) (*ProviderModelMeta, error) {
	if !supportedModelMetaProvider(providerID) {
		return nil, fmt.Errorf("unsupported Models.dev provider %q", providerID)
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("decode Models.dev catalog: %w", err)
	}
	if providers == nil {
		return nil, errors.New("decode Models.dev catalog: expected provider object")
	}
	providerData, ok := providers[providerID]
	if !ok {
		return nil, fmt.Errorf("Models.dev catalog has no %s provider", providerID)
	}
	var provider struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(providerData, &provider); err != nil {
		return nil, fmt.Errorf("decode Models.dev %s provider: %w", providerID, err)
	}
	if provider.Models == nil {
		return nil, fmt.Errorf("Models.dev %s provider has no models object", providerID)
	}
	if len(provider.Models) == 0 {
		return nil, fmt.Errorf("Models.dev %s provider has an empty models object", providerID)
	}

	meta := EmptyProviderModelMeta(providerID)
	for modelID, data := range provider.Models {
		if strings.TrimSpace(modelID) == "" {
			return nil, fmt.Errorf("Models.dev %s provider contains an empty model ID", providerID)
		}
		var modelObject map[string]json.RawMessage
		if err := json.Unmarshal(data, &modelObject); err != nil || modelObject == nil {
			return nil, fmt.Errorf("decode Models.dev model %q: expected model object", modelID)
		}
		var model struct {
			ReasoningOptions []json.RawMessage `json:"reasoning_options"`
		}
		if err := json.Unmarshal(data, &model); err != nil {
			return nil, fmt.Errorf("decode Models.dev model %q: %w", modelID, err)
		}
		options := ProviderModelOptions{}
		for _, raw := range model.ReasoningOptions {
			var option struct {
				Type   string          `json:"type"`
				Values json.RawMessage `json:"values"`
			}
			if err := json.Unmarshal(raw, &option); err != nil {
				// A malformed or future option is not a usable selector control.
				continue
			}
			switch strings.ToLower(strings.TrimSpace(option.Type)) {
			case "effort":
				var values []string
				if len(option.Values) == 0 || json.Unmarshal(option.Values, &values) != nil {
					continue
				}
				for _, value := range values {
					if choice, ok := normalizeThinkingChoice(value); ok {
						options.Effort = appendUnique(options.Effort, choice)
					}
				}
			case "toggle":
				options.Toggle = true
			}
		}
		meta.Models[modelID] = options
	}
	return meta, nil
}

// LoadProviderModelMeta reuses a valid cache without network access. If there
// is no valid cache, it fetches and stores a new snapshot. On failure, an empty
// snapshot and the error are returned so callers can continue without choices.
func LoadProviderModelMeta(ctx context.Context, cachePath, provider string, client HTTPDoer) (*ProviderModelMeta, error) {
	if !supportedModelMetaProvider(provider) {
		return EmptyProviderModelMeta(provider), fmt.Errorf("unsupported Models.dev provider %q", provider)
	}
	if cachePath == "" {
		var err error
		cachePath, err = ProviderModelMetaCachePath(provider)
		if err != nil {
			return EmptyProviderModelMeta(provider), err
		}
	}
	if cached, err := readProviderModelMeta(cachePath, provider); err == nil {
		return cached, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// Invalid or unreadable cache data is treated as a cache miss. A failed
		// fetch will leave the original file in place for diagnosis/recovery.
	}
	meta, err := FetchProviderModelMeta(ctx, provider, client)
	if err != nil {
		return EmptyProviderModelMeta(provider), err
	}
	if err := writeProviderModelMeta(cachePath, meta); err != nil {
		return meta, fmt.Errorf("write %s model metadata cache: %w", provider, err)
	}
	return meta, nil
}

// RefreshProviderModelMeta always fetches and validates a new snapshot before
// atomically replacing the existing cache. A failed refresh preserves the last
// good cache.
func RefreshProviderModelMeta(ctx context.Context, cachePath, provider string, client HTTPDoer) (*ProviderModelMeta, error) {
	if !supportedModelMetaProvider(provider) {
		return nil, fmt.Errorf("unsupported Models.dev provider %q", provider)
	}
	if cachePath == "" {
		var err error
		cachePath, err = ProviderModelMetaCachePath(provider)
		if err != nil {
			return nil, err
		}
	}
	meta, err := FetchProviderModelMeta(ctx, provider, client)
	if err != nil {
		return nil, err
	}
	if err := writeProviderModelMeta(cachePath, meta); err != nil {
		return nil, fmt.Errorf("write %s model metadata cache: %w", provider, err)
	}
	return meta, nil
}

// FetchProviderModelMeta retrieves and parses the public Models.dev catalog.
func FetchProviderModelMeta(ctx context.Context, provider string, client HTTPDoer) (*ProviderModelMeta, error) {
	if !supportedModelMetaProvider(provider) {
		return nil, fmt.Errorf("unsupported Models.dev provider %q", provider)
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Models.dev catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("fetch Models.dev catalog: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsDevCatalogSize+1))
	if err != nil {
		return nil, fmt.Errorf("read Models.dev catalog: %w", err)
	}
	if len(data) > maxModelsDevCatalogSize {
		return nil, fmt.Errorf("Models.dev catalog exceeds %d bytes", maxModelsDevCatalogSize)
	}
	meta, err := ParseProviderModelMeta(data, provider)
	if err != nil {
		return nil, err
	}
	meta.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	return meta, nil
}

func readProviderModelMeta(path, expectedProvider string) (*ProviderModelMeta, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxModelMetaCacheSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxModelMetaCacheSize {
		return nil, fmt.Errorf("model metadata cache exceeds %d bytes", maxModelMetaCacheSize)
	}
	var meta ProviderModelMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decode model metadata cache: %w", err)
	}
	if meta.Provider != expectedProvider {
		return nil, fmt.Errorf("model metadata cache is for provider %q, want %q", meta.Provider, expectedProvider)
	}
	if err := validateProviderModelMeta(&meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func writeProviderModelMeta(path string, meta *ProviderModelMeta) error {
	if err := validateProviderModelMeta(meta); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".model-meta-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func validateProviderModelMeta(meta *ProviderModelMeta) error {
	if meta == nil || meta.Version != ProviderModelMetaVersion {
		return fmt.Errorf("unsupported model metadata cache version")
	}
	if !supportedModelMetaProvider(meta.Provider) {
		return fmt.Errorf("model metadata cache has an unsupported provider %q", meta.Provider)
	}
	if meta.Models == nil {
		return errors.New("model metadata cache has no models object")
	}
	for modelID, options := range meta.Models {
		if strings.TrimSpace(modelID) == "" {
			return errors.New("model metadata cache contains an empty model ID")
		}
		seen := make(map[string]bool, len(options.Effort))
		for _, value := range options.Effort {
			choice, ok := normalizeThinkingChoice(value)
			if !ok || choice != value || seen[value] {
				return fmt.Errorf("model metadata cache has an invalid effort choice for %q", modelID)
			}
			seen[value] = true
		}
	}
	return nil
}

func supportedModelMetaProvider(provider string) bool {
	return provider == "openai" || provider == "opencode-go"
}

func normalizeThinkingChoice(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return value, true
}

func appendUnique(values []string, candidate string) []string {
	for _, value := range values {
		if value == candidate {
			return values
		}
	}
	return append(values, candidate)
}
