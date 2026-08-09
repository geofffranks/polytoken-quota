package target

import (
	"errors"
	"path"
	"strings"
	"unicode"

	"github.com/geofffranks/polytoken-quota/internal/document"
	"gopkg.in/yaml.v3"
)

const maxDefinitionNameRunes = 96

// DefinitionMetadata is the sanitized display and managed-model metadata read
// from one resolver-approved definition file.
type DefinitionMetadata struct {
	Name           string
	Model          string
	FallbackModels []string
}

type definitionMetadataWire struct {
	Name      string `yaml:"name"`
	Polytoken *struct {
		Model          string   `yaml:"model"`
		FallbackModels []string `yaml:"fallback_models"`
	} `yaml:"polytoken"`
}

// ReadDefinitionMetadata reads exactly one resolver-approved file through an
// anchored root and verifies its filesystem identity before reading. It extracts
// only display and managed-model metadata; arbitrary frontmatter and body content
// never cross the target package boundary.
func ReadDefinitionMetadata(definition ResolvedDefinition) (DefinitionMetadata, error) {
	raw, err := readResolvedDefinition(definition)
	if err != nil {
		return DefinitionMetadata{}, definitionError(definition.TargetID, definition.PolicyPath, errors.New("read failed"))
	}
	block, ok := document.Frontmatter(raw)
	if !ok {
		return DefinitionMetadata{Name: pathFallback(definition.PolicyPath)}, nil
	}
	var wire definitionMetadataWire
	if err := yaml.Unmarshal(block, &wire); err != nil {
		return DefinitionMetadata{}, definitionError(definition.TargetID, definition.PolicyPath, errors.New("invalid frontmatter"))
	}
	metadata := DefinitionMetadata{Name: sanitizeDisplayName(wire.Name)}
	if metadata.Name == "" {
		metadata.Name = pathFallback(definition.PolicyPath)
	}
	if wire.Polytoken != nil {
		metadata.Model = wire.Polytoken.Model
		metadata.FallbackModels = append([]string(nil), wire.Polytoken.FallbackModels...)
	}
	return metadata, nil
}

func pathFallback(policyPath string) string {
	base := normalizePublicIdentity(path.Base(policyPath))
	withoutExt := normalizePublicIdentity(strings.TrimSuffix(base, path.Ext(base)))
	if withoutExt != "" {
		return withoutExt
	}
	if base != "" {
		return base
	}
	return "definition"
}

func sanitizeDisplayName(value string) string {
	clean := normalizeLabel(value, 0)
	lower := strings.ToLower(clean)
	redactAt := len(clean)
	for _, marker := range []string{"api_key", "apikey", "authorization", "bearer", "credential", "secret", "token"} {
		if at := strings.Index(lower, marker); at >= 0 && at < redactAt {
			redactAt = at
		}
	}
	clean = strings.TrimSpace(clean[:redactAt])
	return normalizeLabel(clean, maxDefinitionNameRunes)
}

func normalizePublicIdentity(value string) string {
	return normalizeLabel(value, maxDefinitionNameRunes)
}

func normalizeLabel(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	lastSpace := false
	runes := 0
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if b.Len() > 0 && !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
		runes++
		if maxRunes > 0 && runes >= maxRunes {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
