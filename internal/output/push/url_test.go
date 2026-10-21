package push

import (
	"errors"
	"testing"
)

func TestDescribeError(t *testing.T) {
	raw := errors.New(`bad result: &{5 16777216 onStatus 5 [<nil> [{level error} {code NetStream.Publish.Unauthorized} {description authentication failed}]]}`)
	want := "the target refused the stream key (NetStream.Publish.Unauthorized: authentication failed)"
	if got := describeError(raw); got != want {
		t.Fatalf("got %q", got)
	}
	if got := describeError(errors.New("connection refused")); got != "connection refused" {
		t.Fatalf("got %q", got)
	}
}
