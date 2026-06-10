package fa

import (
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
)

// Form wraps a submitted request's values with a fluent validator. Validation
// accumulates one error message per field (not fail-fast), so a whole form's
// problems surface at once and bind to the stdlib FieldError facet by field name.
//
//	f := fa.NewForm(ctx.R)
//	f.Required("email", "Email is required").Email("email", "Enter a valid email")
//	f.Required("password", "Required").MinLen("password", 8, "At least 8 characters")
//	if !f.Valid() {
//	    // re-render the form facet with f.Errors and f.Values
//	}
type Form struct {
	Values map[string]string // submitted field values (first value per field)
	Errors map[string]string // field → first error message
	r      *http.Request
}

const defaultMaxMemory = 32 << 20 // 32 MB for multipart parsing

// NewForm parses the request body (urlencoded or multipart) and returns a Form.
func NewForm(r *http.Request) *Form {
	f := &Form{Values: map[string]string{}, Errors: map[string]string{}, r: r}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		_ = r.ParseMultipartForm(defaultMaxMemory)
	} else {
		_ = r.ParseForm()
	}
	for k, v := range r.Form {
		if len(v) > 0 {
			f.Values[k] = v[0]
		}
	}
	return f
}

// FormFromPayload builds a Form from an event payload (the data-* map a client
// action carries), so handlers validate inline-edited fields the same way.
func FormFromPayload(payload map[string]string) *Form {
	vals := make(map[string]string, len(payload))
	for k, v := range payload {
		vals[k] = v
	}
	return &Form{Values: vals, Errors: map[string]string{}}
}

// Get returns a submitted value, trimmed of surrounding whitespace.
func (f *Form) Get(field string) string { return strings.TrimSpace(f.Values[field]) }

// fail records the first error for a field (later checks on the same field are
// ignored, so the message a developer lists first wins).
func (f *Form) fail(field, msg string) {
	if _, exists := f.Errors[field]; !exists {
		f.Errors[field] = msg
	}
}

// Required fails if the field is empty/whitespace. Chainable.
func (f *Form) Required(field, msg string) *Form {
	if f.Get(field) == "" {
		f.fail(field, msg)
	}
	return f
}

// MinLen fails if the field is shorter than n runes (only when non-empty, so it
// composes with Required without double-reporting). Chainable.
func (f *Form) MinLen(field string, n int, msg string) *Form {
	if v := f.Get(field); v != "" && len([]rune(v)) < n {
		f.fail(field, msg)
	}
	return f
}

// MaxLen fails if the field is longer than n runes. Chainable.
func (f *Form) MaxLen(field string, n int, msg string) *Form {
	if v := f.Get(field); len([]rune(v)) > n {
		f.fail(field, msg)
	}
	return f
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// Email fails if the field is set but not a plausible email. Chainable.
func (f *Form) Email(field, msg string) *Form {
	if v := f.Get(field); v != "" && !emailRe.MatchString(v) {
		f.fail(field, msg)
	}
	return f
}

// Matches fails if the field doesn't match a regular expression. Chainable.
func (f *Form) Matches(field string, re *regexp.Regexp, msg string) *Form {
	if v := f.Get(field); v != "" && !re.MatchString(v) {
		f.fail(field, msg)
	}
	return f
}

// Confirm fails if two fields differ (e.g. password / password_confirm).
func (f *Form) Confirm(field, other, msg string) *Form {
	if f.Get(field) != f.Get(other) {
		f.fail(field, msg)
	}
	return f
}

// Check records msg on field when ok is false — an escape hatch for custom rules
// (uniqueness, business logic). Chainable.
func (f *Form) Check(field string, ok bool, msg string) *Form {
	if !ok {
		f.fail(field, msg)
	}
	return f
}

// Valid reports whether the form passed every check.
func (f *Form) Valid() bool { return len(f.Errors) == 0 }

// Error returns the message for a field (for binding to a FieldError facet), or "".
func (f *Form) Error(field string) string { return f.Errors[field] }

// File returns an uploaded file for a multipart field (nil, nil, err if absent).
func (f *Form) File(field string) (multipart.File, *multipart.FileHeader, error) {
	if f.r == nil {
		return nil, nil, http.ErrMissingFile
	}
	return f.r.FormFile(field)
}
