package resource

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

var (
	errSome  = errors.New("some err")
	errDummy = errors.New("dummy error")
)

type FakeResource struct {
	id string
}

func (f *FakeResource) ID() string {
	return f.id
}
func (f *FakeResource) GetTitle() string { return "title" }

func (f *FakeResource) GetMeta() meta { return meta{"foo": "bar"} }

var stringTests = []struct {
	in, in2 any
	want    int
}{
	{"", "", SUCCESS},
	{"foo", "foo", SUCCESS},
	{"foo", "bar", FAIL},
	{"foo", "", FAIL},
	{true, true, SUCCESS},
}

func TestValidateValue(t *testing.T) {
	for _, c := range stringTests {
		inFunc := func() (any, error) {
			return c.in2, nil
		}
		got := ValidateValue(&FakeResource{""}, "", c.in, inFunc, false)
		if got.Result != c.want {
			t.Errorf("%+v: got %v, want %v", c, got.Result, c.want)
		}
	}
}

func TestValidateValueErr(t *testing.T) {
	for _, c := range stringTests {
		inFunc := func() (any, error) {
			return c.in2, errSome
		}
		got := ValidateValue(&FakeResource{""}, "", c.in, inFunc, false)
		if got.Result != FAIL {
			t.Errorf("%+v: got %v, want %v", c, got.Result, FAIL)
		}
	}
}

func TestValidateValueSkip(t *testing.T) {
	for _, c := range stringTests {
		inFunc := func() (any, error) {
			return c.in2, nil
		}
		got := ValidateValue(&FakeResource{""}, "", c.in, inFunc, true)
		if got.Result != SKIP {
			t.Errorf("%+v: got %v, want %v", c, got.Result, SKIP)
		}
	}
}

func BenchmarkValidateValue(b *testing.B) {
	inFunc := func() (any, error) {
		return "foo", nil
	}
	for n := 0; n < b.N; n++ {
		ValidateValue(&FakeResource{""}, "", "foo", inFunc, false)
	}
}

var containsTests = []struct {
	in   []interface{}
	in2  string
	want int
}{
	{[]interface{}{""}, "", SUCCESS},
	{[]interface{}{"foo"}, "foo\nbar", SUCCESS},
	{[]interface{}{"!foo"}, "foo\nbar", FAIL},
	{[]interface{}{"!moo"}, "foo\nbar", SUCCESS},
	{[]interface{}{"/fo.*/"}, "foo\nbar", SUCCESS},
	{[]interface{}{"!/fo.*/"}, "foo\nbar", FAIL},
	{[]interface{}{"!/mo.*/"}, "foo\nbar", SUCCESS},
	{[]interface{}{"foo"}, "", FAIL},
	{[]interface{}{`/\s/tmp\b/`}, "test /tmp bar", SUCCESS},
}

func TestValidateContains(t *testing.T) {
	for _, c := range containsTests {
		inFunc := func() (io.Reader, error) {
			reader := strings.NewReader(c.in2)
			return reader, nil
		}
		got := ValidateValue(&FakeResource{""}, "", c.in, inFunc, false)
		if got.Result != c.want {
			t.Errorf("%+v: got %v, want %v", c, got.Result, c.want)
		}
	}
}

func TestValidateContainsErr(t *testing.T) {
	for _, c := range containsTests {
		inFunc := func() (io.Reader, error) {
			reader := strings.NewReader(c.in2)
			return reader, errSome
		}
		got := ValidateValue(&FakeResource{""}, "", c.in, inFunc, false)
		if got.Result != FAIL {
			t.Errorf("%+v: got %v, want %v", c, got.Result, FAIL)
		}
	}
}

func TestValidateContainsBadRegexErr(t *testing.T) {
	inFunc := func() (io.Reader, error) {
		reader := strings.NewReader("dummy")
		return reader, nil
	}
	got := ValidateValue(&FakeResource{""}, "", []interface{}{"/*\\.* @@.*/"}, inFunc, false)
	if got.Err == nil {
		t.Errorf("Expected bad regex to raise error, got nil")
	}
}

// TestValidateContainsFailureActual asserts that when a file-contents check fails,
// the MatcherResult.Actual field contains the readable file content — not the
// Go internal type representation "object: *bytes.Reader" / "object: *os.File".
func TestValidateContainsFailureActual(t *testing.T) {
	fileContent := "Banner /etc/issue.net\nLogLevel INFO\n"
	missingPattern := "nonexistent-pattern-xyz"

	inFunc := func() (io.Reader, error) {
		return strings.NewReader(fileContent), nil
	}
	got := ValidateValue(&FakeResource{""}, "contents", []any{missingPattern}, inFunc, false)

	if got.Result != FAIL {
		t.Fatalf("expected FAIL, got %v", got.Result)
	}
	actual, ok := got.MatcherResult.Actual.(string)
	if !ok {
		t.Fatalf("MatcherResult.Actual must be a string, got %T: %v", got.MatcherResult.Actual, got.MatcherResult.Actual)
	}
	if strings.Contains(actual, "object:") {
		t.Errorf("MatcherResult.Actual must not contain Go type repr, got: %q", actual)
	}
	if !strings.Contains(actual, "Banner") {
		t.Errorf("MatcherResult.Actual must contain file content, got: %q", actual)
	}
}

func TestValidateContainsSkip(t *testing.T) {
	for _, c := range containsTests {
		inFunc := func() (io.Reader, error) {
			reader := strings.NewReader(c.in2)
			return reader, nil
		}
		got := ValidateValue(&FakeResource{""}, "", c.in, inFunc, true)
		if got.Result != SKIP {
			t.Errorf("%+v: got %v, want %v", c, got.Result, SKIP)
		}
	}
}

func TestResultMarshaling(t *testing.T) {
	inFunc := func() (io.Reader, error) {
		return nil, errDummy
	}
	res := ValidateValue(&FakeResource{}, "", []string{"x"}, inFunc, false)
	if res.Err == nil {
		t.Fatalf("Expected to receive an error")
	}
	if res.Err.Error() != "dummy error" {
		t.Fatalf("expected to receive 'dummy error', got: %v", res.Err.Error())
	}

	rj, _ := json.Marshal(res)
	res = TestResult{}
	err := json.Unmarshal(rj, &res)
	if err != nil {
		t.Fatalf("could not unmarshal result: %v", err)
	}

	if res.Err == nil {
		t.Fatalf("Expected to receive an error")
	}
	if res.Err.Error() != "dummy error" {
		t.Fatalf("expected to receive 'dummy error', got: %v", res.Err.Error())
	}
}

// trackingReadCloser counts Close calls on a content reader. With readErr
// set, it serves the wrapped content and then fails the read that would
// otherwise report EOF, mimicking a read error partway through the file.
type trackingReadCloser struct {
	reader  io.Reader
	readErr error
	closed  int
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if r.readErr != nil && err == io.EOF {
		err = r.readErr
	}
	return n, err
}

func (r *trackingReadCloser) Close() error {
	r.closed++
	return nil
}

// Every content check must close the reader it opened, whether the match
// passes or fails, for both the pattern-list and single-value matcher shapes.
func TestValidateValueClosesReaderAfterContentCheck(t *testing.T) {
	cases := []struct {
		name     string
		expected any
		want     int
	}{
		{"patterns-pass", []any{"foo"}, SUCCESS},
		{"patterns-fail", []any{"this-pattern-is-absent"}, FAIL},
		{"scalar-pass", "foo\nbar", SUCCESS},
		{"scalar-fail", "this-pattern-is-absent", FAIL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := &trackingReadCloser{reader: strings.NewReader("foo\nbar")}
			got := ValidateValue(&FakeResource{"id"}, "contents", c.expected,
				func() (io.Reader, error) { return rc, nil }, false)
			if got.Result != c.want {
				t.Fatalf("result = %v, want %v", got.Result, c.want)
			}
			if rc.closed != 1 {
				t.Fatalf("reader closed %d time(s), want exactly 1", rc.closed)
			}
		})
	}
}

// A reader handed back together with an open error must still be released.
func TestValidateValueClosesReaderOnOpenError(t *testing.T) {
	for _, expected := range []any{[]any{"foo"}, "foo"} {
		rc := &trackingReadCloser{reader: strings.NewReader("foo")}
		got := ValidateValue(&FakeResource{"id"}, "contents", expected,
			func() (io.Reader, error) { return rc, errSome }, false)
		if got.Result != FAIL {
			t.Fatalf("%T: result = %v, want FAIL", expected, got.Result)
		}
		if rc.closed != 1 {
			t.Fatalf("%T: reader closed %d time(s), want exactly 1", expected, rc.closed)
		}
	}
}

// A read that fails after delivering some content must still produce one
// failure result and close the reader instead of panicking or leaking it.
func TestValidateValueReadErrorFailsAndCloses(t *testing.T) {
	// Pattern-list shape: the read error must surface as a single FAIL result.
	rc := &trackingReadCloser{reader: strings.NewReader("foo\nbar"), readErr: errDummy}
	got := ValidateValue(&FakeResource{"id"}, "contents", []any{"foo"},
		func() (io.Reader, error) { return rc, nil }, false)
	if got.Result != FAIL {
		t.Fatalf("patterns: result = %v, want FAIL", got.Result)
	}
	if rc.closed != 1 {
		t.Fatalf("patterns: reader closed %d time(s), want exactly 1", rc.closed)
	}

	// Scalar shape: the read error is reported directly on the failing result.
	rc2 := &trackingReadCloser{reader: strings.NewReader("foo\nbar"), readErr: errDummy}
	got = ValidateValue(&FakeResource{"id"}, "contents", "foo",
		func() (io.Reader, error) { return rc2, nil }, false)
	if got.Result != FAIL || got.Err == nil || got.Err.Error() != "dummy error" {
		t.Fatalf("scalar: got result = %v, err = %v, want FAIL with %q", got.Result, got.Err, errDummy)
	}
	if rc2.closed != 1 {
		t.Fatalf("scalar: reader closed %d time(s), want exactly 1", rc2.closed)
	}
}
