package stub

import (
	"fmt"
	"strings"

	"google.golang.org/grpc/metadata"
)

// compileMetadata validates response metadata at load time. Values for keys
// ending in -bin are raw bytes carried in a Go/YAML string and are therefore
// exempt from the printable-ASCII rule applied to ordinary metadata values.
func compileMetadata(values map[string]string) (metadata.MD, error) {
	if values == nil {
		return nil, nil
	}
	out := make(metadata.MD, len(values))
	for _, originalKey := range sortedKeys(values) {
		key := strings.ToLower(originalKey)
		if err := validateMetadataKey(key); err != nil {
			return nil, fmt.Errorf("metadata key %q: %w", originalKey, err)
		}
		value := values[originalKey]
		if !strings.HasSuffix(key, "-bin") {
			for _, b := range []byte(value) {
				if b < 0x20 || b > 0x7e {
					return nil, fmt.Errorf("metadata key %q: value contains non-printable ASCII byte 0x%02x", originalKey, b)
				}
			}
		}
		out[key] = append(out[key], value)
	}
	return out, nil
}

func validateMetadataKey(key string) error {
	if key == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.HasPrefix(key, "grpc-") {
		return fmt.Errorf("reserved grpc- prefix is not allowed")
	}
	for _, b := range []byte(key) {
		if (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || b == '_' || b == '-' || b == '.' {
			continue
		}
		return fmt.Errorf("contains invalid byte 0x%02x; allowed characters are 0-9, a-z, _, -, and .", b)
	}
	return nil
}
