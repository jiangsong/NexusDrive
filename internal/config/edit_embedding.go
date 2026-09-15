package config

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// EmbeddingSecretKey is the secret store key `cloudfs index auth` writes
// the embedding API key under, and so the target of the keyring: or
// secretfile: reference it puts in index.embedding.api_key. One fixed key:
// a second auth replaces the first, the way rotating a password does.
const EmbeddingSecretKey = "index.embedding"

// SaveEmbeddingAPIKey stores value in the secret store and points
// index.embedding.api_key at it, editing only that node of the YAML. The
// key itself never enters the configuration file; the reference written
// is returned. The rest of the block is left as it is, and provider and
// model may be set before or after the key: a file that is invalid only
// because its openai block has no api_key yet is what this command
// exists to fix, so the reference is seeded before the file is parsed.
func SaveEmbeddingAPIKey(configPath, value string) (ref string, err error) {
	value = strings.TrimRight(value, "\r\n")
	if value == "" {
		return "", errors.New("config: the embedding api key is empty")
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("config: the embedding api key must be one line")
	}
	if err := validSecret(EmbeddingSecretKey, value); err != nil {
		return "", err
	}
	seed := func(root *yaml.Node) error {
		idx, err := ensureMapping(root, "index")
		if err != nil {
			return err
		}
		emb, err := ensureMapping(idx, "embedding")
		if err != nil {
			return err
		}
		if n := mappingValue(emb, "api_key"); n == nil || n.Value == "" {
			setNode(emb, "api_key", scalar("keyring:"+EmbeddingSecretKey))
		}
		return nil
	}
	err = editConfigNode(configPath, false, seed, func(root *yaml.Node, c *Config) error {
		ref, err = NewSecretStore(c).Put(EmbeddingSecretKey, value)
		if err != nil {
			return err
		}
		setNode(mappingValue(mappingValue(root, "index"), "embedding"), "api_key", scalar(ref))
		return nil
	})
	if err != nil {
		return "", err
	}
	return ref, nil
}

// ensureMapping returns the mapping under key, creating an empty one (or
// replacing an explicit null) when there is none, and refusing a scalar
// or sequence that is already there.
func ensureMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	n := mappingValue(parent, key)
	if n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		n.Kind, n.Tag, n.Value, n.Content = yaml.MappingNode, "!!map", "", nil
	}
	if n == nil {
		n = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		parent.Content = append(parent.Content, scalar(key), n)
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config: %s must be a mapping", key)
	}
	return n, nil
}
