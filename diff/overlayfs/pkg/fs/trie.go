package fs

import (
	"path/filepath"
)

// For now, we keep a simple trie implementation here.
// In the future, we might want to move it to a separate package.
// Use relative path as key, and Node as value.
type TrieKey string

func Parts(fullPath string) []string {
	parts := []string{}
	for _, part := range filepath.SplitList(fullPath) {
		if part != "" && part != string(filepath.Separator) {
			parts = append(parts, part)
		}
	}
	return parts
}

func CheckRemoved(fileMeta FileMeta) (bool, error) {
	whiteout, err := isWhiteout(fileMeta.FullPath)
	if err != nil {
		return false, err
	}
	opaque, err := isOpaqueDir(fileMeta.FullPath)
	if err != nil {
		return false, err
	}
	return whiteout || opaque, nil
}

type OverlayFSTrie struct {
	root   *Trie
	layers []string
}

func NewOverlayFSTrie(layers []string) *OverlayFSTrie {
	root := NewTrie()
	return &OverlayFSTrie{
		root:   root,
		layers: layers,
	}
}

func (t *OverlayFSTrie) Merge(fileMeta FileMeta) (ok bool, err error) {
	parts := Parts(fileMeta.FullPath)
	layerIndex := fileMeta.LayerIndex
	current := t.root
	merged := true
	for i := 0; i < len(parts); i++ {
		key := TrieKey(parts[i])
		if current.children == nil {
			current.children = make(map[TrieKey]*Trie)
		}
		child, ok := current.children[key]
		if !ok {
			child = NewTrie()
			current.children[key] = child
		}
		if child.removed && child.value.LayerIndex >= layerIndex {
			merged = false
			break
		}
		current = child
	}

	if merged {
		current.value = Node{
			FileMeta: FileMeta{
				FullPath:   fileMeta.FullPath,
				LayerIndex: layerIndex,
			},
		}
		if removed, err := CheckRemoved(fileMeta); err != nil {
			//TODO: add log
			return false, err
		} else if removed {
			current.removed = true
			current.DeleteChildrenLazy()
		}
	}
	return merged, nil
}

func (t *OverlayFSTrie) Walk(walkFunc func(node Node) error) error {
	var walk func(t *Trie) error
	walk = func(t *Trie) error {
		if err := walkFunc(t.value); err != nil {
			return err
		}
		for _, child := range t.children {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(t.root)
}

func (t *OverlayFSTrie) GetLayer(layerIndex int) string {
	if layerIndex < 0 || layerIndex >= len(t.layers) {
		return ""
	}
	return t.layers[layerIndex]
}

func (t *OverlayFSTrie) Get(fullPath string) (Node, bool) {
	parts := Parts(fullPath)
	current := t.root
	for _, part := range parts {
		key := TrieKey(part)
		if current.children == nil {
			return Node{}, false
		}
		child, ok := current.children[key]
		if !ok {
			return Node{}, false
		}
		current = child
	}
	return current.value, true
}

type Trie struct {
	children map[TrieKey]*Trie
	value    Node
	removed  bool
}

func (t *Trie) Has(key TrieKey) bool {
	if t.children == nil {
		return false
	}
	_, ok := t.children[key]
	return ok
}

func (t *Trie) Delete(key TrieKey) {
	if t.children == nil {
		return
	}
	delete(t.children, key)
	if len(t.children) == 0 {
		t.children = nil
	}
}

func (t *Trie) DeleteChildren() {
	if t.children == nil {
		return
	}
	for _, child := range t.children {
		child.DeleteChildren()
	}
	t.children = nil
}

func (t *Trie) DeleteChildrenLazy() {
	t.children = nil
}

func NewTrie() *Trie {
	return &Trie{
		children: make(map[TrieKey]*Trie),
		value: Node{
			FileMeta: FileMeta{
				FullPath:   "",
				LayerIndex: -1,
			},
		},
	}
}
