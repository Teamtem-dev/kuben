package protocol_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// Ported from crates/kuben-agent/src/protocol.rs. The byte fixtures are
// written from the serde attributes of the Rust types (the tag first,
// members in declaration order, `skip_serializing_if` members left out,
// serde_json's escaping); the Rust registry was not available to generate
// them, see TestEveryMessageEncodesAsSerdeDid.

func ptr(s string) *string { return &s }

func hello() protocol.Hello {
	return protocol.Hello{
		ProtocolVersions:  []uint32{protocol.ProtocolVersion},
		AgentVersion:      "1.1.2",
		ClusterID:         "0199a0c0-0000-7000-8000-000000000001",
		Capabilities:      protocol.NewFeatures("applicationRuntime"),
		KubernetesVersion: ptr("v1.32.2"),
	}
}

func frame(body string) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	return append(out, body...)
}

func read(t *testing.T, r io.Reader) (protocol.Message, bool) {
	t.Helper()
	m, ok, err := protocol.ReadFrame(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return m, ok
}

func TestFramesRoundTripAndACleanCloseEndsTheStream(t *testing.T) {
	var stream bytes.Buffer
	for _, m := range []protocol.Message{hello(), protocol.Heartbeat{Seq: 7}} {
		if err := protocol.WriteFrame(&stream, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []protocol.Message{hello(), protocol.Heartbeat{Seq: 7}} {
		got, ok := read(t, &stream)
		if !ok {
			t.Fatal("closed early")
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatal(diff)
		}
	}
	if m, ok := read(t, &stream); ok {
		t.Fatalf("a clean close reads as the end, got %#v", m)
	}
}

func TestMessagesHaveAStableCamelCaseShape(t *testing.T) {
	if got := string(protocol.Encode(protocol.Heartbeat{Seq: 1})); got != `{"type":"heartbeat","seq":1}` {
		t.Fatal(got)
	}
	var h map[string]any
	if err := json.Unmarshal(protocol.Encode(hello()), &h); err != nil {
		t.Fatal(err)
	}
	if h["type"] != "hello" || h["clusterId"] != "0199a0c0-0000-7000-8000-000000000001" {
		t.Fatal(h)
	}
	if diff := cmp.Diff([]any{float64(protocol.ProtocolVersion)}, h["protocolVersions"]); diff != "" {
		t.Fatal(diff)
	}
	refused := string(protocol.Encode(protocol.Refused{Reason: protocol.RefusalUnsupportedCapability, Message: "no"}))
	if !strings.Contains(refused, `"reason":"unsupportedCapability"`) {
		t.Fatal(refused)
	}
}

func TestAnUnknownMessageOrReasonDoesNotBreakTheLink(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(frame(`{"type":"futureThing","anything":[1,2,3]}`))
	stream.Write(frame(`{"type":"refused","reason":"futureReason","message":"x"}`))
	if m, _ := read(t, &stream); m != (protocol.Unknown{}) {
		t.Fatalf("%#v", m)
	}
	if m, _ := read(t, &stream); m != (protocol.Refused{Reason: protocol.RefusalOther, Message: "x"}) {
		t.Fatalf("%#v", m)
	}
}

func TestAnOversizedFrameIsRefusedBeforeItIsRead(t *testing.T) {
	// Only the header: reading the body would fail with a link error.
	header := binary.BigEndian.AppendUint32(nil, protocol.MaxFrame+1)
	_, _, err := protocol.ReadFrame(bytes.NewReader(header))
	var tooLarge protocol.TooLargeError
	if !errors.As(err, &tooLarge) || tooLarge != (protocol.TooLargeError{Len: protocol.MaxFrame + 1, Max: protocol.MaxFrame}) {
		t.Fatalf("expected TooLarge, got %v", err)
	}
	if tooLarge.Error() != "a frame of 1048577 bytes exceeds the 1048576-byte limit" {
		t.Fatal(tooLarge.Error())
	}

	huge := protocol.Refused{Reason: protocol.RefusalBadRequest, Message: strings.Repeat("x", protocol.MaxFrame)}
	var sink bytes.Buffer
	if err := protocol.WriteFrame(&sink, huge); !errors.As(err, &tooLarge) {
		t.Fatalf("expected TooLarge, got %v", err)
	}
	if sink.Len() != 0 {
		t.Fatal("nothing is written")
	}
}

func TestAStreamThatEndsInsideAFrameIsAnError(t *testing.T) {
	for name, stream := range map[string][]byte{
		"in the body":   append(binary.BigEndian.AppendUint32(nil, 10), `{"ty`...),
		"in the header": {0, 0},
	} {
		_, _, err := protocol.ReadFrame(bytes.NewReader(stream))
		var link protocol.LinkError
		if !errors.As(err, &link) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: expected a link error, got %v", name, err)
		}
	}
}

func TestAFrameThatIsNotAMessageIsMalformed(t *testing.T) {
	_, _, err := protocol.ReadFrame(bytes.NewReader(frame(`[]`)))
	var malformed protocol.MalformedError
	if !errors.As(err, &malformed) {
		t.Fatalf("expected Malformed, got %v", err)
	}
}

func TestEnvelopesAndObservationsHaveAStableShape(t *testing.T) {
	apply := protocol.Encode(protocol.Apply{Target: "t", Namespace: "kb-shop-prod", Name: "web", Spec: "{}"})
	if got := string(apply); got != `{"type":"apply","target":"t","namespace":"kb-shop-prod","name":"web","spec":"{}"}` {
		t.Fatal(got)
	}
	observed := protocol.Encode(protocol.Observation{Target: "t", Generation: 3, Phase: protocol.RuntimePhaseReady})
	if got := string(observed); got != `{"type":"observed","target":"t","generation":3,"phase":"ready"}` {
		t.Fatal(got)
	}
	later, err := protocol.Decode([]byte(`{"type":"observed","target":"t","generation":3,"phase":"hibernating"}`))
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := later.(protocol.Observation); !ok || o.Phase != protocol.RuntimePhaseUnknown {
		t.Fatalf("%#v", later)
	}
}

func TestTheHandshakePicksTheHighestCommonVersionAndSharedFeatures(t *testing.T) {
	hub := protocol.NewFeatures("applicationRuntime", "executionTask")
	agreed, err := protocol.Negotiate(protocol.Versions{Min: 1, Max: 2}, hub, []uint32{1, 2, 3},
		protocol.NewFeatures("executionTask", "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if agreed.Version != 2 {
		t.Fatal(agreed.Version)
	}
	if diff := cmp.Diff(protocol.NewFeatures("executionTask"), agreed.Features); diff != "" {
		t.Fatal(diff)
	}
	if err := agreed.Require("executionTask"); err != nil {
		t.Fatal("both know it")
	}
	for _, f := range []string{"logs", "applicationRuntime"} {
		if err := agreed.Require(f); !errors.Is(err, protocol.RefusalUnsupportedCapability) {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := protocol.Negotiate(protocol.Versions{Min: 2, Max: 3}, hub, []uint32{1}, nil); !errors.Is(err, protocol.RefusalUnsupportedProtocol) {
		t.Fatal(err)
	}
}

func TestABuildSpeaksItsVersionAndTheOneBefore(t *testing.T) {
	v := protocol.SupportedVersions
	if !v.Contains(protocol.ProtocolVersion) || v.Min < 1 || protocol.ProtocolVersion-v.Min > 1 {
		t.Fatalf("%+v", v)
	}
	if diff := cmp.Diff([]uint32{protocol.ProtocolVersion}, v.Descending()); diff != "" && v.Min == v.Max {
		t.Fatal(diff)
	}
	if diff := cmp.Diff([]uint32{2, 1}, protocol.Versions{Min: 1, Max: 2}.Descending()); diff != "" {
		t.Fatal(diff)
	}
}

// The wire form of the Rust `Apply` (serde camelCase).
func TestApplyWireForm(t *testing.T) {
	data, err := json.Marshal(protocol.Apply{Target: "t", Namespace: "kb-shop-prod", Name: "web", Spec: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"target":"t","namespace":"kb-shop-prod","name":"web","spec":"{}"}` {
		t.Fatal(got)
	}
}

// Every message, byte for byte as serde_json::to_vec wrote it, and back.
func TestEveryMessageEncodesAsSerdeDid(t *testing.T) {
	csr := "-----BEGIN CERTIFICATE REQUEST-----\nMIIB\n-----END CERTIFICATE REQUEST-----\n"
	cases := []struct {
		msg  protocol.Message
		want string
	}{
		{hello(), `{"type":"hello","protocolVersions":[1],"agentVersion":"1.1.2",` +
			`"clusterId":"0199a0c0-0000-7000-8000-000000000001","capabilities":["applicationRuntime"],"kubernetesVersion":"v1.32.2"}`},
		{
			protocol.Hello{ProtocolVersions: []uint32{2, 1}, AgentVersion: "2.0.0", ClusterID: "c"},
			`{"type":"hello","protocolVersions":[2,1],"agentVersion":"2.0.0","clusterId":"c","capabilities":[]}`,
		},
		{
			protocol.Welcome{ProtocolVersion: 1, HubVersion: "1.2.0", HeartbeatMs: 10000, Features: protocol.NewFeatures("b", "a")},
			`{"type":"welcome","protocolVersion":1,"hubVersion":"1.2.0","heartbeatMs":10000,"features":["a","b"]}`,
		},
		{
			protocol.Welcome{ProtocolVersion: 1, HubVersion: "h", HeartbeatMs: 20, Features: protocol.Features{}},
			`{"type":"welcome","protocolVersion":1,"hubVersion":"h","heartbeatMs":20,"features":[]}`,
		},
		{
			protocol.Refused{Reason: protocol.RefusalInvalidToken, Message: "no"},
			`{"type":"refused","reason":"invalidToken","message":"no"}`,
		},
		{protocol.Heartbeat{Seq: 18446744073709551615}, `{"type":"heartbeat","seq":18446744073709551615}`},
		{protocol.HeartbeatAck{Seq: 7}, `{"type":"heartbeatAck","seq":7}`},
		{
			protocol.Enroll{Token: protocol.NewToken("kbt_ab"), ClusterID: "primary", CSR: csr},
			`{"type":"enroll","token":"kbt_ab","clusterId":"primary","csr":"-----BEGIN CERTIFICATE REQUEST-----\nMIIB\n-----END CERTIFICATE REQUEST-----\n"}`,
		},
		{protocol.Renew{CSR: "x"}, `{"type":"renew","csr":"x"}`},
		{protocol.Enrolled{Certificate: "c", NotAfter: 1789086400}, `{"type":"enrolled","certificate":"c","notAfter":1789086400}`},
		{
			protocol.Apply{Target: "t", Namespace: "n", Name: "w", Spec: `{"env":"a<b&c>d"}`},
			`{"type":"apply","target":"t","namespace":"n","name":"w","spec":"{\"env\":\"a<b&c>d\"}"}`,
		},
		{
			protocol.Observation{Target: "t", Generation: -1, Phase: protocol.RuntimePhaseRejected, Reason: ptr("DigestMismatch"), Message: ptr("m")},
			`{"type":"observed","target":"t","generation":-1,"phase":"rejected","reason":"DigestMismatch","message":"m"}`,
		},
		{protocol.Unknown{}, `{"type":"unknown"}`},
	}
	for _, c := range cases {
		got := protocol.Encode(c.msg)
		if string(got) != c.want {
			t.Errorf("%T:\n got %s\nwant %s", c.msg, got, c.want)
		}
		back, err := protocol.Decode(got)
		if err != nil {
			t.Errorf("%T: %v", c.msg, err)
			continue
		}
		want := c.msg
		if h, ok := want.(protocol.Hello); ok && h.Capabilities == nil {
			h.Capabilities = protocol.Features{}
			want = h
		}
		if diff := cmp.Diff(want, back, cmp.AllowUnexported(protocol.Token{})); diff != "" {
			t.Errorf("%T: %s", c.msg, diff)
		}
	}
	// A frame is the big-endian length and the body.
	var stream bytes.Buffer
	if err := protocol.WriteFrame(&stream, protocol.Heartbeat{Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(append([]byte{0, 0, 0, 28}, `{"type":"heartbeat","seq":1}`...), stream.Bytes()); diff != "" {
		t.Fatal(diff)
	}
}

// serde_json escapes `"`, `\` and control characters only.
func TestStringsEscapeAsSerdeJSON(t *testing.T) {
	got := string(protocol.Encode(protocol.Renew{CSR: "\x00\x01\x1f\x7f\u2028\u2029é<>&\"\\\b\f\n\r\t/"}))
	want := `{"type":"renew","csr":"\u0000\u0001\u001f` + "\x7f\u2028\u2029é<>&" + `\"\\\b\f\n\r\t/"}`
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	back, err := protocol.Decode([]byte(got))
	if err != nil || back.(protocol.Renew).CSR != "\x00\x01\x1f\x7f\u2028\u2029é<>&\"\\\b\f\n\r\t/" {
		t.Fatalf("%#v %v", back, err)
	}
}

// Decoding refuses what serde refused and takes what it took.
func TestDecodingIsAsStrictAsSerde(t *testing.T) {
	refused := []string{
		``, `null`, `[]`, `"hello"`, `{}`, `{"type":1}`, `{"type":null}`,
		`{"type":"heartbeat"}`, `{"type":"heartbeat","seq":-1}`, `{"type":"heartbeat","seq":1.0}`,
		`{"type":"heartbeat","seq":1e2}`, `{"type":"heartbeat","seq":"1"}`, `{"type":"heartbeat","seq":null}`,
		`{"type":"heartbeat","seq":18446744073709551616}`,
		`{"type":"welcome","protocolVersion":4294967296,"hubVersion":"h","heartbeatMs":1,"features":[]}`,
		`{"type":"welcome","protocolVersion":1,"hubVersion":"h","heartbeatMs":1}`,
		`{"type":"hello","protocolVersions":[1],"agentVersion":"a","clusterId":"c","capabilities":null}`,
		`{"type":"hello","protocolVersions":[1],"agentVersion":"a"}`,
		`{"type":"refused","reason":1,"message":"x"}`,
		`{"type":"enroll","token":null,"clusterId":"c","csr":"x"}`,
		`{"type":"observed","target":"t","generation":1}`,
		`{"type":"heartbeat","seq":1} x`,
		"{\"type\":\"renew\",\"csr\":\"\xff\"}",
	}
	for _, body := range refused {
		if m, err := protocol.Decode([]byte(body)); err == nil {
			t.Errorf("%s: accepted as %#v", body, m)
		}
	}
	accepted := map[string]protocol.Message{
		`{"seq":3,"type":"heartbeat","extra":true}`: protocol.Heartbeat{Seq: 3},
		`{"type":"hello","protocolVersions":[1,1],"agentVersion":"a","clusterId":"c","kubernetesVersion":null}`: protocol.Hello{
			ProtocolVersions: []uint32{1, 1}, AgentVersion: "a", ClusterID: "c", Capabilities: protocol.Features{},
		},
		`{"type":"hello","protocolVersions":[],"agentVersion":"a","clusterId":"c","capabilities":["b","a","b"]}`: protocol.Hello{
			ProtocolVersions: []uint32{}, AgentVersion: "a", ClusterID: "c", Capabilities: protocol.Features{"a", "b"},
		},
		`{"type":"observed","target":"t","generation":2,"phase":"failed","reason":null}`: protocol.Observation{
			Target: "t", Generation: 2, Phase: protocol.RuntimePhaseFailed,
		},
		"\t{\"type\":\"unknown\"}\n":        protocol.Unknown{},
		`{"type":"renamedLater","seq":"x"}`: protocol.Unknown{},
	}
	for body, want := range accepted {
		got, err := protocol.Decode([]byte(body))
		if err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s: %s", body, diff)
		}
	}
}

func TestATokenNeverShowsInFormatting(t *testing.T) {
	var buf bytes.Buffer
	token := protocol.NewToken("kbt_secret")
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		buf.Reset()
		fmt.Fprintf(&buf, verb, protocol.Enroll{Token: token, ClusterID: "primary"})
		if strings.Contains(buf.String(), "kbt_secret") {
			t.Errorf("%s: %s", verb, buf.String())
		}
	}
	if token.Expose() != "kbt_secret" {
		t.Fatal("the token is kept")
	}
}
