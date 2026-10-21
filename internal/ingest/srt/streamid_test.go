package srt

import "testing"

func TestParseStreamID(t *testing.T) {
	cases := []struct {
		in   string
		want StreamID
	}{
		{"live/tv1", StreamID{ModePublish, "live/tv1", ""}},
		{"live/tv1?key=abc", StreamID{ModePublish, "live/tv1", "abc"}},
		{"publish:live/tv1?key=a%20b", StreamID{ModePublish, "live/tv1", "a b"}},
		{"read:live/tv1", StreamID{ModeRead, "live/tv1", ""}},
		{"#!::r=live/tv1,m=publish,s=xyz", StreamID{ModePublish, "live/tv1", "xyz"}},
		{"#!::m=request,r=live/tv1", StreamID{ModeRead, "live/tv1", ""}},
		{"#!::r=live/tv1,key=k1", StreamID{ModePublish, "live/tv1", "k1"}},
	}
	for _, c := range cases {
		got, err := ParseStreamID(c.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %+v, want %+v", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "  ", "#!::m=publish", "#!::r=a/b,m=bogus", "publish:"} {
		if _, err := ParseStreamID(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
