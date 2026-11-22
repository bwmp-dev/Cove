package worker

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

func normalizeComposeFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	closeErr := file.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if len(data) > 1<<20 {
		return "", errors.New("Compose file exceeds 1 MiB")
	}
	var document yaml.Node
	if err = yaml.Unmarshal(data, &document); err != nil {
		return "", err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return path, nil
	}
	changed := false
	root := document.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "networks" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		networks := root.Content[i+1]
		for j := 1; j < len(networks.Content); j += 2 {
			network := networks.Content[j]
			if network.Kind != yaml.MappingNode {
				continue
			}
			for k := 0; k+1 < len(network.Content); k += 2 {
				if network.Content[k].Value == "name" && network.Content[k+1].Tag == "!!null" {
					network.Content = append(network.Content[:k], network.Content[k+2:]...)
					changed = true
					break
				}
			}
		}
	}
	if !changed {
		return path, nil
	}
	data, err = yaml.Marshal(&document)
	if err != nil {
		return "", err
	}
	// Keep relative include, extends, env_file and build paths beside the original file.
	normalized, err := os.CreateTemp(filepath.Dir(path), ".cove-normalized-*.yaml")
	if err != nil {
		return "", err
	}
	_, writeErr := normalized.Write(data)
	closeErr = normalized.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return normalized.Name(), nil
}
