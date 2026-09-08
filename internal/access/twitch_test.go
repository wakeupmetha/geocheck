package access

import (
	"errors"
	"strings"
	"testing"
)

// gqlReply wraps a token payload the way Twitch does: a JSON document inside a
// JSON string field. Building it here rather than pasting escaped fixtures
// keeps the cases readable.
func gqlReply(t *testing.T, payload string) string {
	t.Helper()
	quoted := strings.ReplaceAll(payload, `"`, `\"`)
	return `{"data":{"streamPlaybackAccessToken":{"value":"` + quoted +
		`","signature":"deadbeef","__typename":"PlaybackAccessToken"}}}`
}

// servedPayload is the shape of a token granted normally, trimmed to the fields
// the check reads.
const servedPayload = `{"authorization":{"forbidden":false,"reason":""},` +
	`"ci_gb":false,"geoblock_reason":"","channel":"twitch"}`

// proxyReason is the sentence the player shows when Twitch decides the address
// is a proxy. This is the finding the check exists for.
const proxyReason = "A proxy or unblocker has been detected. This premium " +
	"content will not be available to you while it is in use. (Error #3)"

func TestClassifyTwitchToken(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		want       State
		wantDetail string
		wantToken  bool
	}{
		{
			name:      "a granted token is available and carries the token on",
			status:    200,
			body:      gqlReply(t, servedPayload),
			want:      StateAvailable,
			wantToken: true,
		},
		{
			name:   "the proxy refusal is reported as such",
			status: 200,
			body: gqlReply(t, `{"authorization":{"forbidden":true,"reason":"`+
				proxyReason+`"},"ci_gb":false,"geoblock_reason":""}`),
			want:       StateBlocked,
			wantDetail: twitchProxyDetail,
		},
		{
			// The sentence is matched before anything is decoded, so a refusal
			// that arrives in some other shape is still caught.
			name:       "the sentence is caught outside the token payload",
			status:     403,
			body:       `[{"error":"` + proxyReason + `"}]`,
			want:       StateBlocked,
			wantDetail: twitchProxyDetail,
		},
		{
			// A geographic block is a different finding and must not be
			// reported as a proxy detection.
			name:   "a geoblock is not a proxy detection",
			status: 200,
			body: gqlReply(t, `{"authorization":{"forbidden":false,"reason":""},`+
				`"ci_gb":true,"geoblock_reason":"country"}`),
			want:       StateBlocked,
			wantDetail: "geoblocked: country",
		},
		{
			name:   "a refusal without a reason still blocks",
			status: 200,
			body: gqlReply(t, `{"authorization":{"forbidden":true,"reason":""},`+
				`"ci_gb":false,"geoblock_reason":""}`),
			want:       StateBlocked,
			wantDetail: "playback forbidden",
		},
		{
			name:   "a gql error is not a verdict",
			status: 200,
			body:   `{"errors":[{"message":"service unavailable"}]}`,
			want:   StateError,
		},
		{
			name:   "a missing token is not a verdict",
			status: 200,
			body:   `{"data":{"streamPlaybackAccessToken":null}}`,
			want:   StateError,
		},
		{
			name:   "unexpected status",
			status: 502,
			body:   "bad gateway",
			want:   StateError,
		},
		{
			name:   "unreadable body",
			status: 200,
			body:   "not json",
			want:   StateError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, value, sig := classifyTwitchToken(tc.status, tc.body)
			if res.State != tc.want {
				t.Fatalf("state = %v, want %v (detail %q)", res.State, tc.want, res.Detail)
			}
			if tc.wantDetail != "" && res.Detail != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", res.Detail, tc.wantDetail)
			}
			if got := value != "" && sig != ""; got != tc.wantToken {
				t.Fatalf("token carried on = %v, want %v", got, tc.wantToken)
			}
		})
	}
}

func TestTwitchHostUp(t *testing.T) {
	// The statuses these hosts answer with in normal operation. None of them
	// means the host is unreachable, which is the whole point of the helper.
	for _, status := range []int{200, 301, 302, 403, 404, 405} {
		if !twitchHostUp(status, nil) {
			t.Errorf("HTTP %d should count as reachable", status)
		}
	}
	if twitchHostUp(503, nil) {
		t.Error("HTTP 503 should not count as reachable")
	}
	if twitchHostUp(0, errors.New("dial failed")) {
		t.Error("a transport failure should not count as reachable")
	}
}

func TestTwitchEndpointResult(t *testing.T) {
	if got := twitchEndpointResult(12, nil); got.State != StateAvailable {
		t.Errorf("all up = %v, want available", got.State)
	}
	got := twitchEndpointResult(12, []string{"usher.ttvnw.net"})
	if got.State != StateRestricted {
		t.Errorf("one down = %v, want restricted", got.State)
	}
	if !strings.Contains(got.Detail, "usher.ttvnw.net") {
		t.Errorf("detail %q should name the failing host", got.Detail)
	}
	all := twitchEndpointResult(2, []string{"a", "b"})
	if all.State != StateBlocked {
		t.Errorf("all down = %v, want blocked", all.State)
	}
}
