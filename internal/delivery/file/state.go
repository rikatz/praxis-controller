package file

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/praxis-proxy/controller/internal/config"
)

const stateVersion = 1

type ownerState struct {
	Epoch         string    `json:"epoch"`
	RetiredEpochs []string  `json:"retiredEpochs,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type storedState struct {
	Version      int              `json:"version"`
	Target       config.Target    `json:"target"`
	OwnerEpoch   string           `json:"ownerEpoch"`
	ConfigDigest string           `json:"configDigest"`
	Resources    []storedResource `json:"resources"`
}

type storedResource struct {
	Key       config.Key       `json:"key"`
	Revision  string           `json:"revision"`
	Source    config.ObjectRef `json:"source,omitempty"`
	ValueKind config.Kind      `json:"valueKind"`
	Value     json.RawMessage  `json:"value"`
}

func toStored(resource config.Resource) (storedResource, error) {
	value, err := json.Marshal(resource.Value)
	if err != nil {
		return storedResource{}, fmt.Errorf("marshal resource value: %w", err)
	}
	return storedResource{
		Key:       resource.Key,
		Revision:  resource.Revision,
		Source:    resource.Source,
		ValueKind: resource.Key.Kind,
		Value:     value,
	}, nil
}

func fromStored(stored storedResource) (config.Resource, error) {
	value, err := decodeValue(stored.ValueKind, stored.Value)
	if err != nil {
		return config.Resource{}, err
	}
	resource, err := config.NewResource(stored.Key, stored.Source, value)
	if err != nil {
		return config.Resource{}, err
	}
	if resource.Revision != stored.Revision {
		return config.Resource{}, fmt.Errorf("stored resource %q revision does not match its value", stored.Key.String())
	}
	return resource, nil
}

func decodeValue(kind config.Kind, data []byte) (config.Value, error) {
	switch kind {
	case config.KindListener:
		var value config.Listener
		return value, json.Unmarshal(data, &value)
	case config.KindHTTPRoute:
		var value config.HTTPRouteTable
		return value, json.Unmarshal(data, &value)
	case config.KindCluster:
		var value config.Cluster
		return value, json.Unmarshal(data, &value)
	case config.KindEndpointSet:
		var value config.EndpointSet
		return value, json.Unmarshal(data, &value)
	case config.KindSecretHandle:
		var value config.SecretHandle
		return value, json.Unmarshal(data, &value)
	default:
		return nil, fmt.Errorf("unsupported stored kind %q", kind)
	}
}

func loadState(dir string) (storedState, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(err) {
		return storedState{}, false, nil
	}
	if err != nil {
		return storedState{}, false, err
	}
	// An unreadable or inconsistent state is treated as absent so a later
	// full-snapshot apply can repair it. Partial applies are still rejected
	// when no listener is present, so this cannot clobber a good document.
	var state storedState
	if err := json.Unmarshal(data, &state); err != nil {
		return storedState{}, false, nil
	}
	if state.Version != stateVersion {
		return storedState{}, false, nil
	}
	for _, resource := range state.Resources {
		if _, err := fromStored(resource); err != nil {
			return storedState{}, false, nil
		}
	}
	return state, true, nil
}

func loadOwner(dir string) (ownerState, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "owner.json"))
	if os.IsNotExist(err) {
		return ownerState{}, false, nil
	}
	if err != nil {
		return ownerState{}, false, err
	}
	var owner ownerState
	if err := json.Unmarshal(data, &owner); err != nil {
		return ownerState{}, false, fmt.Errorf("owner file is corrupt: %w", err)
	}
	return owner, true, nil
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

func writeState(dir string, state storedState) error {
	sort.Slice(state.Resources, func(i, j int) bool {
		return state.Resources[i].Key.String() < state.Resources[j].Key.String()
	})
	return writeJSON(filepath.Join(dir, "state.json"), state)
}

// writeAtomic replaces path with data using a temporary file in the same
// directory, fsyncs it, renames it, and fsyncs the directory.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// secureJoin builds a path under root and rejects any existing component that
// is a symlink or not a directory.
func secureJoin(root string, parts ...string) (string, error) {
	current := root
	for _, part := range parts {
		if !validComponent(part) {
			return "", fmt.Errorf("invalid path component %q", part)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path component %q is a symlink", current)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("path component %q is not a directory", current)
		}
	}
	return current, nil
}

func validComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.':
		default:
			return false
		}
	}
	return true
}
