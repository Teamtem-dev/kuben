package agentlink

// The messages' JSON, written as serde_json wrote the Rust enum
// (`#[serde(tag = "type", rename_all = "camelCase", rename_all_fields =
// "camelCase")]`): the tag first, then the members in declaration order,
// `Option`s with `skip_serializing_if` left out when absent, sets sorted,
// and serde's string escaping (only `"`, `\` and control characters; not
// `<`, `>`, `&` or U+2028/2029, which encoding/json escapes). Reading is as
// strict as serde: required members, exact integer types, unknown members
// ignored, an unknown tag or enum value read as the `other` variant.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// Encode is the JSON of m, as serde_json::to_vec wrote it.
func Encode(m Message) []byte {
	var e encoder
	e.buf.WriteString(`{"type":`)
	e.str(m.messageType())
	switch m := m.(type) {
	case Hello:
		e.hello(m)
	case Welcome:
		e.welcome(m)
	case Refused:
		e.fields(field{"reason", string(m.Reason)}, field{"message", m.Message})
	case Heartbeat:
		e.key("seq")
		e.uint(m.Seq)
	case Enroll:
		e.fields(field{"token", m.Token.secret}, field{"clusterId", m.ClusterID}, field{"csr", m.CSR})
	case Renew:
		e.fields(field{"csr", m.CSR})
	case Enrolled:
		e.fields(field{"certificate", m.Certificate})
		e.key("notAfter")
		e.buf.WriteString(strconv.FormatInt(m.NotAfter, 10))
	case HeartbeatAck:
		e.key("seq")
		e.uint(m.Seq)
	case Apply:
		e.fields(field{"target", m.Target}, field{"namespace", m.Namespace}, field{"name", m.Name}, field{"spec", m.Spec})
	case Observation:
		e.observation(m)
	case Unknown:
	}
	e.buf.WriteByte('}')
	return e.buf.Bytes()
}

// field is a string member.
type field struct{ name, value string }

func (e *encoder) fields(fs ...field) {
	for _, f := range fs {
		e.key(f.name)
		e.str(f.value)
	}
}

func (e *encoder) hello(m Hello) {
	e.key("protocolVersions")
	e.u32s(m.ProtocolVersions)
	e.fields(field{"agentVersion", m.AgentVersion}, field{"clusterId", m.ClusterID})
	e.key("capabilities")
	e.set(m.Capabilities)
	e.optStr("kubernetesVersion", m.KubernetesVersion)
}

func (e *encoder) welcome(m Welcome) {
	e.key("protocolVersion")
	e.uint(uint64(m.ProtocolVersion))
	e.fields(field{"hubVersion", m.HubVersion})
	e.key("heartbeatMs")
	e.uint(m.HeartbeatMs)
	e.key("features")
	e.set(m.Features)
}

func (e *encoder) observation(m Observation) {
	e.fields(field{"target", m.Target})
	e.key("generation")
	e.buf.WriteString(strconv.FormatInt(m.Generation, 10))
	e.fields(field{"phase", string(m.Phase)})
	e.optStr("reason", m.Reason)
	e.optStr("message", m.Message)
}

type encoder struct {
	buf bytes.Buffer
}

func (e *encoder) key(name string) {
	e.buf.WriteByte(',')
	e.str(name)
	e.buf.WriteByte(':')
}

func (e *encoder) uint(v uint64) { e.buf.WriteString(strconv.FormatUint(v, 10)) }

func (e *encoder) u32s(vs []uint32) {
	e.buf.WriteByte('[')
	for i, v := range vs {
		if i > 0 {
			e.buf.WriteByte(',')
		}
		e.uint(uint64(v))
	}
	e.buf.WriteByte(']')
}

func (e *encoder) set(f Features) {
	e.buf.WriteByte('[')
	for i, name := range NewFeatures(f...) {
		if i > 0 {
			e.buf.WriteByte(',')
		}
		e.str(name)
	}
	e.buf.WriteByte(']')
}

func (e *encoder) optStr(name string, v *string) {
	if v != nil {
		e.key(name)
		e.str(*v)
	}
}

const hexDigits = "0123456789abcdef"

// str writes s as serde_json's format_escaped_str did.
func (e *encoder) str(s string) {
	e.buf.WriteByte('"')
	start := 0
	for i := range len(s) {
		c := s[i]
		var esc string
		switch {
		case c == '"':
			esc = `\"`
		case c == '\\':
			esc = `\\`
		case c == '\b':
			esc = `\b`
		case c == '\f':
			esc = `\f`
		case c == '\n':
			esc = `\n`
		case c == '\r':
			esc = `\r`
		case c == '\t':
			esc = `\t`
		case c < 0x20:
			esc = `\u00` + string(hexDigits[c>>4]) + string(hexDigits[c&0xf])
		default:
			continue
		}
		e.buf.WriteString(s[start:i])
		e.buf.WriteString(esc)
		start = i + 1
	}
	e.buf.WriteString(s[start:])
	e.buf.WriteByte('"')
}

// Decode reads one message as serde_json::from_slice read the Rust enum.
func Decode(body []byte) (Message, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("invalid UTF-8")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("invalid type: expected an internally tagged enum Message")
	}
	var f fields
	if err := json.Unmarshal(trimmed, &f); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	raw, ok := f["type"]
	if !ok {
		return nil, errors.New("missing field `type`")
	}
	tag, err := decodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("type: %w", err)
	}
	return f.variant(tag)
}

// fields are the members of a message object.
type fields map[string]json.RawMessage

func (f fields) variant(tag string) (Message, error) {
	switch tag {
	case "hello":
		return f.hello()
	case "welcome":
		return f.welcome()
	case "refused":
		return f.refused()
	case "heartbeat":
		seq, err := f.u64("seq")
		return Heartbeat{Seq: seq}, err
	case "heartbeatAck":
		seq, err := f.u64("seq")
		return HeartbeatAck{Seq: seq}, err
	case "enroll":
		return f.enroll()
	case "renew":
		csr, err := f.str("csr")
		return Renew{CSR: csr}, err
	case "enrolled":
		return f.enrolled()
	case "apply":
		return f.apply()
	case "observed":
		return f.observed()
	}
	return Unknown{}, nil
}

func (f fields) hello() (Message, error) {
	var h Hello
	var err error
	if h.ProtocolVersions, err = f.u32s("protocolVersions"); err != nil {
		return nil, err
	}
	if h.AgentVersion, err = f.str("agentVersion"); err != nil {
		return nil, err
	}
	if h.ClusterID, err = f.str("clusterId"); err != nil {
		return nil, err
	}
	if h.Capabilities, err = f.set("capabilities", false); err != nil {
		return nil, err
	}
	if h.KubernetesVersion, err = f.optStr("kubernetesVersion"); err != nil {
		return nil, err
	}
	return h, nil
}

func (f fields) welcome() (Message, error) {
	var w Welcome
	var err error
	if w.ProtocolVersion, err = f.u32("protocolVersion"); err != nil {
		return nil, err
	}
	if w.HubVersion, err = f.str("hubVersion"); err != nil {
		return nil, err
	}
	if w.HeartbeatMs, err = f.u64("heartbeatMs"); err != nil {
		return nil, err
	}
	if w.Features, err = f.set("features", true); err != nil {
		return nil, err
	}
	return w, nil
}

func (f fields) refused() (Message, error) {
	reason, err := f.str("reason")
	if err != nil {
		return nil, err
	}
	message, err := f.str("message")
	if err != nil {
		return nil, err
	}
	return Refused{Reason: ParseRefusal(reason), Message: message}, nil
}

func (f fields) enroll() (Message, error) {
	token, err := f.str("token")
	if err != nil {
		return nil, err
	}
	cluster, err := f.str("clusterId")
	if err != nil {
		return nil, err
	}
	csr, err := f.str("csr")
	if err != nil {
		return nil, err
	}
	return Enroll{Token: NewToken(token), ClusterID: cluster, CSR: csr}, nil
}

func (f fields) enrolled() (Message, error) {
	certificate, err := f.str("certificate")
	if err != nil {
		return nil, err
	}
	notAfter, err := f.i64("notAfter")
	if err != nil {
		return nil, err
	}
	return Enrolled{Certificate: certificate, NotAfter: notAfter}, nil
}

func (f fields) apply() (Message, error) {
	var a Apply
	for _, m := range []struct {
		name string
		to   *string
	}{{"target", &a.Target}, {"namespace", &a.Namespace}, {"name", &a.Name}, {"spec", &a.Spec}} {
		v, err := f.str(m.name)
		if err != nil {
			return nil, err
		}
		*m.to = v
	}
	return a, nil
}

func (f fields) observed() (Message, error) {
	var o Observation
	var err error
	if o.Target, err = f.str("target"); err != nil {
		return nil, err
	}
	if o.Generation, err = f.i64("generation"); err != nil {
		return nil, err
	}
	phase, err := f.str("phase")
	if err != nil {
		return nil, err
	}
	o.Phase = ParseRuntimePhase(phase)
	if o.Reason, err = f.optStr("reason"); err != nil {
		return nil, err
	}
	if o.Message, err = f.optStr("message"); err != nil {
		return nil, err
	}
	return o, nil
}

func (f fields) required(name string) (json.RawMessage, error) {
	raw, ok := f[name]
	if !ok {
		return nil, fmt.Errorf("missing field `%s`", name)
	}
	return raw, nil
}

func (f fields) str(name string) (string, error) {
	raw, err := f.required(name)
	if err != nil {
		return "", err
	}
	s, err := decodeString(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return s, nil
}

// optStr is an Option<String> with `default`: absent or null is nil.
func (f fields) optStr(name string) (*string, error) {
	raw, ok := f[name]
	if !ok || isNull(raw) {
		return nil, nil //nolint:nilnil // absent is nil, as serde's None
	}
	s, err := decodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &s, nil
}

func (f fields) u64(name string) (uint64, error) {
	raw, err := f.required(name)
	if err != nil {
		return 0, err
	}
	return decodeUint(name, raw, 64)
}

func (f fields) u32(name string) (uint32, error) {
	raw, err := f.required(name)
	if err != nil {
		return 0, err
	}
	v, err := decodeUint(name, raw, 32)
	return uint32(v), err //nolint:gosec // parsed with a 32-bit bound
}

func (f fields) i64(name string) (int64, error) {
	raw, err := f.required(name)
	if err != nil {
		return 0, err
	}
	if !isNumber(raw) {
		return 0, fmt.Errorf("%s: invalid type: expected i64", name)
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid value: expected i64", name)
	}
	return v, nil
}

func (f fields) u32s(name string) ([]uint32, error) {
	raw, err := f.required(name)
	if err != nil {
		return nil, err
	}
	items, err := decodeArray(name, raw)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, 0, len(items))
	for _, item := range items {
		v, err := decodeUint(name, item, 32)
		if err != nil {
			return nil, err
		}
		out = append(out, uint32(v)) //nolint:gosec // parsed with a 32-bit bound
	}
	return out, nil
}

// set is a BTreeSet<String>: required, or `default` (none) when absent.
func (f fields) set(name string, required bool) (Features, error) {
	raw, ok := f[name]
	if !ok {
		if required {
			return nil, fmt.Errorf("missing field `%s`", name)
		}
		return Features{}, nil
	}
	items, err := decodeArray(name, raw)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		s, err := decodeString(item)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		names = append(names, s)
	}
	return NewFeatures(names...), nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func isNumber(raw json.RawMessage) bool {
	return len(raw) > 0 && (raw[0] == '-' || ('0' <= raw[0] && raw[0] <= '9'))
}

func decodeString(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", errors.New("invalid type: expected a string")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%w", err)
	}
	return s, nil
}

func decodeUint(name string, raw json.RawMessage, bits int) (uint64, error) {
	raw = bytes.TrimSpace(raw)
	if !isNumber(raw) {
		return 0, fmt.Errorf("%s: invalid type: expected u%d", name, bits)
	}
	v, err := strconv.ParseUint(string(raw), 10, bits)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid value: expected u%d", name, bits)
	}
	return v, nil
}

func decodeArray(name string, raw json.RawMessage) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, fmt.Errorf("%s: invalid type: expected a sequence", name)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return items, nil
}
