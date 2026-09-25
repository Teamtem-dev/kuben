// Package dns is DNS for domain claims (M5.2; crates/kuben-api/src/dns.rs):
// lookups over DNS-over-HTTPS and the first provider adapter, Cloudflare.
//
// Lookups go to a DNS-over-HTTPS resolver (`domains.doh_url`), so a claim
// is checked against the public DNS, not the cluster's view of it.
//
// The provider adapter writes A, AAAA, CNAME and TXT records. It is careful
// with what it did not write:
//   - Every record it creates carries the comment `kuben:<org>`.
//   - A record of the same name and type without that comment is someone
//     else's, and is reported as a conflict instead of being overwritten.
//   - A record is deleted only when the provider still reports the id Kuben
//     recorded for it.
//   - A create whose answer was lost is found again by name, type and
//     comment on the next pass.
//
// The Cloudflare adapter stays hand-written over HTTP (not libdns): the
// ownership rules above need record comments and ids, which libdns does
// not carry.
package dns

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/outbound"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const (
	timeout = 10 * time.Second
	maxBody = 1 << 20
	// TypeNS is the NS record type by number, as DNS-over-HTTPS answers
	// name it.
	TypeNS = 2
	// TypeTXT is the TXT record type by number.
	TypeTXT = 16
)

// ErrorKind is why a DNS operation failed.
type ErrorKind string

// The kinds of failure.
const (
	// Refused means the provider refused the credentials.
	Refused ErrorKind = "refused"
	// Conflict means a record of that name and type exists and is not
	// Kuben's.
	Conflict ErrorKind = "conflict"
	// Unavailable means DNS could not be reached or answered badly.
	Unavailable ErrorKind = "unavailable"
)

// Error is a failed DNS operation (dns.rs DnsError).
type Error struct {
	Kind   ErrorKind
	Detail string
}

// Error is the Rust error's text.
func (e Error) Error() string {
	switch e.Kind {
	case Refused:
		return "the DNS provider refused the credentials: " + e.Detail
	case Conflict:
		return e.Detail + " exists and was not written by Kuben; remove or change it first"
	case Unavailable:
		return "DNS is unavailable: " + e.Detail
	}
	return "DNS is unavailable: " + e.Detail
}

func unavailable(format string, args ...any) error {
	return Error{Kind: Unavailable, Detail: fmt.Sprintf(format, args...)}
}

// DoHAnswers are the answers of type kind in a DNS-over-HTTPS JSON body.
// A name that does not exist (NXDOMAIN) has none.
func DoHAnswers(body []byte, kind uint64) ([]string, error) {
	var parsed struct {
		Status *uint64 `json:"Status"`
		Answer []struct {
			Type *uint64 `json:"type"`
			Data *string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, unavailable("unexpected answer: %v", err)
	}
	if parsed.Status == nil {
		return nil, unavailable("unexpected answer: missing field `Status`")
	}
	switch *parsed.Status {
	case 0, 3:
	default:
		return nil, unavailable("the resolver answered rcode %d", *parsed.Status)
	}
	out := []string{}
	for _, a := range parsed.Answer {
		if a.Type == nil || a.Data == nil {
			return nil, unavailable("unexpected answer: an answer without its type or data")
		}
		if *a.Type != kind {
			continue
		}
		data := strings.TrimSpace(*a.Data)
		if kind == TypeTXT {
			// `"part one" "part two"` → `part onepart two`.
			var b strings.Builder
			for i, part := range strings.Split(data, `"`) {
				if i%2 == 1 {
					b.WriteString(part)
				}
			}
			out = append(out, b.String())
		} else {
			out = append(out, ascii.Lower(strings.TrimRight(data, ".")))
		}
	}
	return out, nil
}

// Zone is a DNS zone at a provider.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// NameServers are the name servers the provider assigned to the zone.
	NameServers []string `json:"name_servers"`
}

// ProviderRecord is a record as the provider reports it.
type ProviderRecord struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	RecordType string          `json:"type"`
	Content    string          `json:"content"`
	Proxied    bool            `json:"proxied"`
	Comment    opt.Val[string] `json:"comment"`
}

// RecordSpec is a record Kuben wants.
type RecordSpec struct {
	Name       string `json:"name"`
	RecordType string `json:"type"`
	Content    string `json:"content"`
}

// Change is what one pass does to one record.
//
//sumtype:decl
type Change interface{ isChange() }

type (
	// Create writes a new record.
	Create struct{ Spec RecordSpec }
	// Update rewrites a record Kuben wrote.
	Update struct {
		ID   string
		Spec RecordSpec
	}
	// Delete removes a record Kuben wrote and no longer wants.
	Delete struct{ ID, Name, RecordType string }
	// Unchanged is a record that is as wanted.
	Unchanged struct {
		ID   string
		Spec RecordSpec
	}
	// ConflictWith is someone else's record standing in the way.
	ConflictWith struct{ What string }
)

func (Create) isChange()       {}
func (Update) isChange()       {}
func (Delete) isChange()       {}
func (Unchanged) isChange()    {}
func (ConflictWith) isChange() {}

// Plan is what makes existing (the provider's records of the wanted names)
// look like wanted. Only records tagged tag are updated or deleted, and a
// deletion only of an id in known (the ids Kuben recorded).
func Plan(wanted []RecordSpec, existing []ProviderRecord, known []string, tag string) []Change {
	ours := func(r ProviderRecord) bool { c, ok := r.Comment.Get(); return ok && c == tag }
	var changes []Change
	var used []string
	for _, spec := range wanted {
		var sameKind []ProviderRecord
		for _, r := range existing {
			if strings.EqualFold(r.Name, spec.Name) && r.RecordType == spec.RecordType {
				sameKind = append(sameKind, r)
			}
		}
		if i := slices.IndexFunc(sameKind, func(r ProviderRecord) bool { return ours(r) && r.Content == spec.Content }); i >= 0 {
			used = append(used, sameKind[i].ID)
			changes = append(changes, Unchanged{ID: sameKind[i].ID, Spec: spec})
			continue
		}
		if i := slices.IndexFunc(sameKind, func(r ProviderRecord) bool { return ours(r) && !slices.Contains(used, r.ID) }); i >= 0 {
			used = append(used, sameKind[i].ID)
			changes = append(changes, Update{ID: sameKind[i].ID, Spec: spec})
			continue
		}
		theirs := slices.ContainsFunc(sameKind, func(r ProviderRecord) bool { return !ours(r) })
		cnameBlocked := spec.RecordType == "CNAME" && slices.ContainsFunc(existing, func(r ProviderRecord) bool {
			return strings.EqualFold(r.Name, spec.Name) && !ours(r)
		})
		if theirs || cnameBlocked {
			changes = append(changes, ConflictWith{What: spec.RecordType + " " + spec.Name})
		} else {
			changes = append(changes, Create{Spec: spec})
		}
	}
	for _, r := range existing {
		if ours(r) && !slices.Contains(used, r.ID) && slices.Contains(known, r.ID) {
			changes = append(changes, Delete{ID: r.ID, Name: r.Name, RecordType: r.RecordType})
		}
	}
	return changes
}

// GatewayRecords are the records pointing host at a gateway: a CNAME to
// cname when set, else A and AAAA records of the public addresses.
func GatewayRecords(host string, addresses []netip.Addr, cname opt.Val[string]) []RecordSpec {
	if target, ok := cname.Get(); ok {
		return []RecordSpec{{Name: host, RecordType: "CNAME", Content: strings.TrimRight(target, ".")}}
	}
	out := []RecordSpec{}
	for _, a := range addresses {
		if outbound.IsPrivate(a) {
			continue
		}
		kind := "AAAA"
		if a.Is4() {
			kind = "A"
		}
		out = append(out, RecordSpec{Name: host, RecordType: kind, Content: a.String()})
	}
	slices.SortFunc(out, func(a, b RecordSpec) int {
		return cmp.Or(strings.Compare(a.RecordType, b.RecordType), strings.Compare(a.Content, b.Content))
	})
	return slices.Compact(out)
}
