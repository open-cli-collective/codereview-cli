package symlinkmetadata

import (
	"bytes"
	"strings"
	"testing"
)

func TestArtifactDigestPinsExactPayloadAndIdentity(t *testing.T) {
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	link := Link{
		OldPath: "assets/old",
		Path:    "assets/new",
		Base:    &Side{Path: "assets/old", Mode: "120000", BlobOID: strings.Repeat("c", 40), Payload: "../target", PayloadSize: len("../target"), Resolution: ResolutionRegular, ResolvedPath: "target", TargetMode: "100644", TargetBlobOID: strings.Repeat("d", 40)},
		Head:    &Side{Path: "assets/new", Mode: "120000", BlobOID: strings.Repeat("c", 40), Payload: "../target", PayloadSize: len("../target"), Resolution: ResolutionMissing, ResolvedPath: "target"},
	}
	artifact, err := New(baseSHA, headSHA, []Link{link})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	data, err := artifact.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, pathLink, err := Decode(data, artifact.Digest, baseSHA, headSHA, "assets/new")
	if err != nil || got.Digest != artifact.Digest || pathLink.Head == nil || pathLink.Head.Payload != "../target" {
		t.Fatalf("Decode = %#v, %#v, %v", got, pathLink, err)
	}
	for name, tc := range map[string]struct {
		digest, base, head, path string
	}{
		"wrong digest":        {strings.Repeat("e", 64), baseSHA, headSHA, "assets/new"},
		"wrong base identity": {artifact.Digest, strings.Repeat("f", 40), headSHA, "assets/new"},
		"wrong head identity": {artifact.Digest, baseSHA, strings.Repeat("f", 40), "assets/new"},
		"unassigned path":     {artifact.Digest, baseSHA, headSHA, "assets/old"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Decode(data, tc.digest, tc.base, tc.head, tc.path); err == nil {
				t.Fatal("Decode succeeded for stale or non-exact input")
			}
		})
	}
	corrupt := bytes.Replace(data, []byte("../target"), []byte("../evil"), 1)
	if _, _, err := Decode(corrupt, artifact.Digest, baseSHA, headSHA, "assets/new"); err == nil {
		t.Fatal("Decode accepted a payload changed after digest creation")
	}
	unknown := append([]byte(nil), data...)
	unknown = bytes.Replace(unknown, []byte(`"digest":`), []byte(`"unknown":true,"digest":`), 1)
	if _, _, err := Decode(unknown, artifact.Digest, baseSHA, headSHA, "assets/new"); err == nil {
		t.Fatal("Decode accepted an unknown artifact field")
	}
}

func TestArtifactPreservesNonUTF8PayloadBytes(t *testing.T) {
	artifact, err := New(strings.Repeat("a", 40), strings.Repeat("b", 40), []Link{{
		Path: "links/current",
		Head: &Side{Path: "links/current", Mode: "120000", BlobOID: strings.Repeat("c", 40), PayloadBase64: "YWJj/w==", PayloadSize: 4, Resolution: ResolutionUnsupported},
	}})
	if err != nil {
		t.Fatalf("New non-UTF8 payload: %v", err)
	}
	data, err := artifact.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, link, err := Decode(data, artifact.Digest, artifact.BaseSHA, artifact.HeadSHA, "links/current"); err != nil || link.Head.PayloadBase64 != "YWJj/w==" {
		t.Fatalf("Decode non-UTF8 payload = %#v, err %v", link, err)
	}
}

func TestArtifactDistinguishesInspectedEmptyPayloadFromOversizeUnavailablePayload(t *testing.T) {
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	empty, err := New(baseSHA, headSHA, []Link{{
		Path: "links/empty",
		Head: &Side{Path: "links/empty", Mode: "120000", BlobOID: strings.Repeat("c", 40), Payload: "", PayloadSize: 0, Resolution: ResolutionUnsupported},
	}})
	if err != nil {
		t.Fatalf("New empty payload: %v", err)
	}
	emptyBytes, err := empty.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(emptyBytes, []byte(`"payload": ""`)) || !bytes.Contains(emptyBytes, []byte(`"payload_size": 0`)) || bytes.Contains(emptyBytes, []byte("payload_omitted_reason")) {
		t.Fatalf("empty payload JSON = %s, want explicit inspected empty bytes without omission reason", emptyBytes)
	}
	if _, link, err := Decode(emptyBytes, empty.Digest, baseSHA, headSHA, "links/empty"); err != nil || link.Head.PayloadSize != 0 || link.Head.PayloadOmittedReason != "" {
		t.Fatalf("Decode empty payload = %#v, err %v", link, err)
	}

	oversize, err := New(baseSHA, headSHA, []Link{{
		Path: "links/oversize",
		Head: &Side{Path: "links/oversize", Mode: "120000", BlobOID: strings.Repeat("d", 40), PayloadSize: MaxPayloadBytes + 1, PayloadOmittedReason: "oversize", Resolution: ResolutionUnsupported},
	}})
	if err != nil {
		t.Fatalf("New oversize unavailable payload: %v", err)
	}
	oversizeBytes, err := oversize.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(oversizeBytes, []byte(`"payload": ""`)) || !bytes.Contains(oversizeBytes, []byte(`"payload_size": 4097`)) || !bytes.Contains(oversizeBytes, []byte(`"payload_omitted_reason": "oversize"`)) {
		t.Fatalf("oversize payload JSON = %s, want explicit unavailable bytes and reason", oversizeBytes)
	}
	if _, link, err := Decode(oversizeBytes, oversize.Digest, baseSHA, headSHA, "links/oversize"); err != nil || link.Head.PayloadOmittedReason != "oversize" {
		t.Fatalf("Decode oversize payload = %#v, err %v", link, err)
	}
	if _, err := New(baseSHA, headSHA, []Link{{
		Path: "links/oversize",
		Head: &Side{Path: "links/oversize", Mode: "120000", BlobOID: strings.Repeat("d", 40), PayloadSize: MaxPayloadBytes + 1, Resolution: ResolutionUnsupported},
	}}); err == nil {
		t.Fatal("New accepted unavailable oversized payload without an explicit omission reason")
	}
}

func TestArtifactRejectsUnsafePathsAndMalformedPinnedObjectIdentity(t *testing.T) {
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	validSide := Side{Path: "links/current", Mode: "120000", BlobOID: strings.Repeat("c", 40), Payload: "../target", PayloadSize: len("../target"), Resolution: ResolutionMissing, ResolvedPath: "target"}
	for _, tc := range []struct {
		name string
		link Link
	}{
		{name: "traversal path", link: Link{Path: "../outside", Head: &Side{Path: "../outside", Mode: "120000", BlobOID: strings.Repeat("c", 40), Payload: "target", PayloadSize: len("target"), Resolution: ResolutionMissing, ResolvedPath: "target"}}},
		{name: "git metadata path", link: Link{Path: "links/.git/config", Head: &Side{Path: "links/.git/config", Mode: "120000", BlobOID: strings.Repeat("c", 40), Payload: "target", PayloadSize: len("target"), Resolution: ResolutionMissing, ResolvedPath: "target"}}},
		{name: "malformed blob oid", link: Link{Path: "links/current", Head: &Side{Path: "links/current", Mode: "120000", BlobOID: "bad", Payload: "../target", PayloadSize: len("../target"), Resolution: ResolutionMissing, ResolvedPath: "target"}}},
		{name: "unknown resolution", link: Link{Path: "links/current", Head: &Side{Path: "links/current", Mode: "120000", BlobOID: strings.Repeat("c", 40), Payload: "../target", PayloadSize: len("../target"), Resolution: "followed", ResolvedPath: "target"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(baseSHA, headSHA, []Link{tc.link}); err == nil {
				t.Fatal("New accepted unsafe path or invalid object identity")
			}
		})
	}
	if _, err := New(baseSHA, headSHA, []Link{{Path: "links/current", Head: &validSide}}); err != nil {
		t.Fatalf("New valid control: %v", err)
	}
}
