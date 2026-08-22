package stub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yinghanhung/simulacra/internal/match"
)

// ParseDocument decodes exactly one stub mapping — the admin API's
// CreateStub grammar — in YAML or JSON form, with the same strictness the
// file loader applies (unknown fields are errors). It returns the decoded
// stub and its normalized document: the input's own node tree re-rendered as
// block YAML with comments and styling stripped. Rendering from the node
// rather than the struct keeps nil-versus-empty distinctions the struct
// cannot represent (an empty message step must stay `message: {}`).
func ParseDocument(data []byte) (Stub, string, error) {
	node, err := decodeSingleDocument(data)
	if err != nil {
		return Stub{}, "", err
	}
	if node.Kind == yaml.SequenceNode {
		return Stub{}, "", errors.New("document is a sequence; the API takes one stub mapping per call (files take lists)")
	}
	if node.Kind != yaml.MappingNode {
		return Stub{}, "", errors.New("document must be a YAML or JSON mapping holding one stub")
	}
	// Strict decode runs from the original bytes, not the node, for two
	// reasons: yaml.Node.Decode has no KnownFields control, and errors must
	// carry the caller's own line numbers.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Stub
	if err := dec.Decode(&s); err != nil {
		return Stub{}, "", fmt.Errorf("parsing stub document: %w", err)
	}
	doc, err := renderNode(node)
	if err != nil {
		return Stub{}, "", err
	}
	return s, doc, nil
}

// decodeSingleDocument parses data into exactly one YAML document node and
// returns its content node. Empty input and multi-document input are errors.
func decodeSingleDocument(data []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("document is empty; expected one stub mapping")
		}
		return nil, fmt.Errorf("parsing document: %w", err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, errors.New("document holds more than one YAML document; expected exactly one stub mapping")
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("document is empty; expected one stub mapping")
	}
	return doc.Content[0], nil
}

// maxAliasDepth bounds alias expansion. YAML permits an anchor to reference
// itself, which would otherwise expand forever.
const maxAliasDepth = 64

// normalizeNode strips comments, styling, and anchors, and expands aliases in
// place, so rendering yields canonical, self-contained block YAML regardless
// of how the input was written.
//
// Expanding aliases is what makes a per-stub document stand alone. A stub
// file may declare an anchor in one list item and alias it from another —
// the whole-file decode resolves that fine — but a single item rendered on
// its own would emit a bare "*name" whose "&name" lives in a sibling
// document, and would fail to parse or export. depth counts expansions, not
// structural nesting.
func normalizeNode(n *yaml.Node, depth int) error {
	if n.Kind == yaml.AliasNode {
		if depth >= maxAliasDepth {
			return fmt.Errorf("YAML alias %q nests more than %d levels deep (recursive anchor?)", n.Value, maxAliasDepth)
		}
		if n.Alias == nil {
			return fmt.Errorf("YAML alias %q has no anchor", n.Value)
		}
		resolved := cloneNode(n.Alias)
		if err := normalizeNode(resolved, depth+1); err != nil {
			return err
		}
		*n = *resolved
		return nil
	}
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	n.Style = 0
	n.Anchor = "" // every alias is expanded, so anchors are dead weight
	for _, c := range n.Content {
		if err := normalizeNode(c, depth); err != nil {
			return err
		}
	}
	return nil
}

// cloneNode deep-copies a node so expanding an alias cannot mutate the
// anchor's own subtree (which other aliases may still reference).
func cloneNode(n *yaml.Node) *yaml.Node {
	clone := *n
	if n.Content != nil {
		clone.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			clone.Content[i] = cloneNode(child)
		}
	}
	return &clone
}

// renderNode renders one mapping node as a normalized document.
func renderNode(n *yaml.Node) (string, error) {
	if err := normalizeNode(n, 0); err != nil {
		return "", err
	}
	out, err := yaml.Marshal(n)
	if err != nil {
		return "", fmt.Errorf("rendering normalized document: %w", err)
	}
	return string(out), nil
}

// RenderSequence assembles normalized per-stub documents into one
// file-grammar YAML sequence (ExportStubs). Assembling nodes rather than
// concatenating indented text keeps block scalars and nesting correct.
func RenderSequence(docs []string) (string, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for i, doc := range docs {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
			return "", fmt.Errorf("document %d: %w", i, err)
		}
		if len(node.Content) == 0 {
			return "", fmt.Errorf("document %d is empty", i)
		}
		seq.Content = append(seq.Content, node.Content[0])
	}
	out, err := yaml.Marshal(seq)
	if err != nil {
		return "", fmt.Errorf("rendering stub sequence: %w", err)
	}
	return string(out), nil
}

// ParseMatchDocument decodes a stub-grammar match block
// (VerifyCalls.matcher_document) with the loader's strictness. Empty or
// whitespace-only input returns a nil block — the match-all value the
// contract requires ("empty matches any call to method"); match.Compile and
// journal.Verify already treat nil as match-all.
func ParseMatchDocument(data []byte) (*match.Block, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	node, err := decodeSingleDocument(data)
	if err != nil {
		return nil, err
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("matcher document must be a mapping (metadata/message/expr)")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var b match.Block
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("parsing matcher document: %w", err)
	}
	return &b, nil
}
