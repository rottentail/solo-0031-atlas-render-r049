// Package statematch implements stateful model-based verification for the
// scene revision / archive / restore / freeze lifecycle.
//
// A hand-written reference model (see model.go) encodes the lifecycle rules
// taken from PROJECT_SPEC.md. Random command sequences are replayed against
// both the reference model and the real service (see executor.go); after
// every command the two states must agree, and a set of standalone
// invariants must hold. Failing sequences are reduced by the shrinker in
// shrink.go.
package statematch

import (
	"atlas-render/internal/model"
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// Fixed identifier pool. The generator only uses valid identifiers so that
// rejection paths are driven by stale revisions, lifecycle state and
// validation failures, never by malformed IDs.
const (
	SceneA            = "scene-alpha"
	SceneB            = "scene-bravo"
	Asset1            = "asset-one"
	Asset2            = "asset-two"
	MissingAsset      = "asset-ghost"
	MissingSnapshotID = "snap-00000000" // service IDs start at snap-00000001
	SceneName         = "verified-scene"
)

// CommandKind enumerates the lifecycle operations under verification.
type CommandKind string

const (
	CmdPutScene    CommandKind = "put_scene"
	CmdSetState    CommandKind = "set_state"
	CmdFreeze      CommandKind = "freeze"
	CmdRevoke      CommandKind = "revoke"
	CmdImportAsset CommandKind = "import_asset"
	CmdRetireAsset CommandKind = "retire_asset"
)

// Command is one generated operation. DocIndex == -1 selects an invalid
// document; SnapshotRef == -1 addresses a snapshot that cannot exist.
type Command struct {
	Kind        CommandKind `json:"kind"`
	SceneID     string      `json:"sceneId,omitempty"`
	AssetID     string      `json:"assetId,omitempty"`
	Expected    int         `json:"expectedRevision,omitempty"`
	Target      string      `json:"targetState,omitempty"`
	DocIndex    int         `json:"docIndex"`
	BadName     bool        `json:"badName,omitempty"`
	SnapshotRef int         `json:"snapshotRef,omitempty"`
}

func (c Command) String() string {
	switch c.Kind {
	case CmdPutScene:
		return "put_scene        scene=" + c.SceneID + " expected=" + itoa(c.Expected) + " doc=" + itoa(c.DocIndex) + boolTag(" badName", c.BadName)
	case CmdSetState:
		return "set_state        scene=" + c.SceneID + " expected=" + itoa(c.Expected) + " target=" + c.Target
	case CmdFreeze:
		return "freeze           scene=" + c.SceneID + " expected=" + itoa(c.Expected)
	case CmdRevoke:
		return "revoke           snapshotRef=" + itoa(c.SnapshotRef)
	case CmdImportAsset:
		return "import_asset     asset=" + c.AssetID
	case CmdRetireAsset:
		return "retire_asset     asset=" + c.AssetID
	default:
		return string(c.Kind)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func boolTag(label string, on bool) string {
	if on {
		return label
	}
	return ""
}

func transform() model.Transform {
	return model.Transform{ScaleX: 1, ScaleY: 1}
}

func rectLayer(id string, fill string) model.Layer {
	return model.Layer{
		ID:        id,
		Kind:      "rectangle",
		Width:     10,
		Height:    10,
		Transform: transform(),
		Opacity:   1,
		Fill:      model.Paint{Kind: "solid", Color: fill},
	}
}

// ValidDocuments returns the document pool the generator submits. The
// documents are deliberately distinct so that an overwritten revision is
// always observable through the document digest.
func ValidDocuments() []model.Document {
	doc0 := model.Document{Width: 32, Height: 24, Background: "#FFFFFF", Layers: []model.Layer{
		rectLayer("plate-0", "#145A70"),
	}}
	doc1 := model.Document{Width: 32, Height: 24, Background: "#000000", Layers: []model.Layer{
		rectLayer("plate-1", "#E87E4F"),
		{
			ID:        "disc-1",
			Kind:      "ellipse",
			Width:     6,
			Height:    6,
			Transform: transform(),
			Opacity:   1,
			Fill:      model.Paint{Kind: "solid", Color: "#32A5A0"},
		},
	}}
	// doc2 closes over Asset1, exercising the freeze-time asset closure and
	// the "references unavailable asset" rejection paths.
	doc2 := model.Document{Width: 32, Height: 24, Background: "#F5F1E8", Layers: []model.Layer{
		rectLayer("plate-2", "#145A70"),
		{
			ID:        "photo-2",
			Kind:      "image",
			Width:     2,
			Height:    2,
			Transform: transform(),
			Opacity:   1,
			AssetID:   Asset1,
		},
	}}
	return []model.Document{doc0, doc1, doc2}
}

// InvalidDocument fails scene.Validate before any transaction opens.
func InvalidDocument() model.Document {
	return model.Document{Width: 0, Height: 24, Background: "#FFFFFF", Layers: []model.Layer{
		rectLayer("bad", "#145A70"),
	}}
}

// TinyPNG returns a deterministic, spec-legal PNG payload (2x2, RGBA).
func TinyPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	img.Set(0, 0, color.RGBA{R: 240, G: 40, B: 80, A: 255})
	var buf bytes.Buffer
	if e := png.Encode(&buf, img); e != nil {
		panic(e)
	}
	return buf.Bytes()
}
