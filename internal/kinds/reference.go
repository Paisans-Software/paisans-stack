package kinds

import "strings"

// Reference is a container image reference, split far enough to tell a pinned
// one from a floating one. It is deliberately not a full parser: the toolkit
// pulls nothing and resolves nothing, so the only question it has to answer is
// whether two runs of `apply` would get the same bytes.
type Reference struct {
	// Name is everything before the tag or digest.
	Name string
	// Tag is the tag, empty when the reference carries a digest or names none.
	Tag string
	// Digest is the `sha256:...` part, empty when there is none.
	Digest string
}

// ParseReference splits a reference. A malformed one is not an error here:
// anything it cannot split comes back with an empty tag and digest, which the
// caller refuses for being unpinned, and that is the answer either way.
func ParseReference(ref string) Reference {
	ref = strings.TrimSpace(ref)
	if at := strings.Index(ref, "@"); at >= 0 {
		return Reference{Name: ref[:at], Digest: ref[at+1:]}
	}
	// A colon before the last slash belongs to a registry port, not a tag.
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return Reference{Name: ref[:colon], Tag: ref[colon+1:]}
	}
	return Reference{Name: ref}
}

// Pinned reports whether a reference names one build.
//
// A digest is the strongest form and always pins. A tag pins by convention
// rather than by construction, since a publisher can move one, but a version
// tag that moves is a publisher breaking its own promise. `latest` makes no
// such promise, and neither does an absent tag, which means `latest` written a
// shorter way.
func (r Reference) Pinned() bool {
	if r.Digest != "" {
		return true
	}
	return r.Tag != "" && r.Tag != "latest"
}

// Floating explains why a reference does not pin, for the refusal message. It
// returns an empty string when the reference is fine.
func (r Reference) Floating() string {
	switch {
	case r.Pinned():
		return ""
	case r.Tag == "latest":
		return "the tag is `latest`"
	case r.Tag == "" && r.Digest == "":
		return "there is no tag, which means `latest`"
	default:
		return "it does not name one build"
	}
}
