package access

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestClassifyKinopoisk(t *testing.T) {
	reply := func(status string) string {
		return `{"data":{"ott":{"contentAvailability":{"geo":{"availabilityStatus":"` + status +
			`","__typename":"GeoContentAvailability"},"__typename":"ContentAvailability"},"__typename":"OttData"}}}`
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   State
	}{
		{"available", 200, reply("AVAILABLE"), StateAvailable},
		{"restricted", 200, reply("RESTRICTED"), StateRestricted},
		{
			// The gateway refuses documents it does not know. That says the
			// query drifted from the client's, not anything about the region.
			name: "unknown document", status: 400,
			body: `{"errors":[{"message":"the query is not allowed"}]}`,
			want: StateError,
		},
		{
			// The web client reads a missing status as available; a check
			// must not guess that way.
			name: "no status", status: 200,
			body: `{"data":{"ott":{"contentAvailability":{"geo":null}}}}`,
			want: StateError,
		},
		{"not json", 404, "Not Found", StateError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyKinopoisk(c.status, c.body); got.State != c.want {
				t.Errorf("state = %v, want %v (detail %q)", got.State, c.want, got.Detail)
			}
		})
	}
}

// vkVideoItem builds one catalogue entry. Stream URLs carry the address they
// were signed for, which is how the check learns what VK saw.
func vkVideoItem(srcIP string) string {
	if srcIP == "" {
		return `{"id":2,"files":{},"content_restricted_message":"withheld"}`
	}
	return `{"id":1,"files":{"hls":"https://vkvd1.okcdn.ru/?srcIp=` + srcIP + `&expires=1","failover_host":"vkvd2.okcdn.ru"}}`
}

func vkCatalogue(items ...string) string {
	return `{"response":{"catalog":{},"videos":[` + strings.Join(items, ",") + `]}}`
}

func TestClassifyVKVideo(t *testing.T) {
	public := netip.MustParseAddr("203.0.113.7")
	elsewhere := "198.51.100.9"

	cases := []struct {
		name       string
		body       string
		want       State
		wantDetail string
	}{
		{
			name: "everything plays",
			body: vkCatalogue(vkVideoItem(public.String()), vkVideoItem(public.String())),
			want: StateAvailable, wantDetail: "2/2 videos playable",
		},
		{
			name: "some titles withheld",
			body: vkCatalogue(vkVideoItem(public.String()), vkVideoItem("")),
			want: StateRestricted, wantDetail: "1/2 videos playable; withheld",
		},
		{
			name: "nothing plays",
			body: vkCatalogue(vkVideoItem(""), vkVideoItem("")),
			want: StateBlocked, wantDetail: "0/2 videos playable; withheld",
		},
		{
			// Russian services are routinely routed out a separate exit, and
			// the verdict is then about an address the report never shows.
			name: "served through another exit",
			body: vkCatalogue(vkVideoItem(elsewhere)),
			want: StateAvailable, wantDetail: "1/1 videos playable; VK saw " + elsewhere,
		},
		{
			name: "api error",
			body: `{"error":{"error_code":5,"error_msg":"User authorization failed"}}`,
			want: StateError,
		},
		{"empty catalogue", vkCatalogue(), StateError, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyVKVideo(200, c.body, public)
			if got.State != c.want {
				t.Errorf("state = %v, want %v (detail %q)", got.State, c.want, got.Detail)
			}
			if c.wantDetail != "" && got.Detail != c.wantDetail {
				t.Errorf("detail = %q, want %q", got.Detail, c.wantDetail)
			}
		})
	}
}

func TestClassifySoundCloudTrack(t *testing.T) {
	track := func(policy string, snipped bool) string {
		s := "false"
		if snipped {
			s = "true"
		}
		return `{"id":293,"policy":"` + policy + `","media":{"transcodings":[` +
			`{"url":"https://api-v2.soundcloud.com/media/soundcloud:tracks:293/x/stream/hls","snipped":` + s + `}]}}`
	}
	cases := []struct {
		name       string
		status     int
		body       string
		wantStream bool
		want       State
	}{
		{"streamable", 200, track("ALLOW", false), true, 0},
		{"blocked in this country", 200, track("BLOCK", false), false, StateBlocked},
		{"previews only", 200, track("SNIP", true), false, StateRestricted},
		{"only a snippet offered", 200, track("ALLOW", true), false, StateError},
		// A 401 means the scraped client_id went stale, not that the address
		// was refused.
		{"stale client_id", 401, `{}`, false, StateError},
		{"server error", 503, "", false, StateError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stream, got := classifySoundCloudTrack(c.status, c.body)
			if (stream != "") != c.wantStream {
				t.Fatalf("stream = %q, want one: %v", stream, c.wantStream)
			}
			if !c.wantStream && got.State != c.want {
				t.Errorf("state = %v, want %v (detail %q)", got.State, c.want, got.Detail)
			}
		})
	}
}

func TestSoundCloudScraping(t *testing.T) {
	page := `<script>window.__sc_hydration = [{"hydratable":"geoip","data":{"country_code":"XA","country_name":"Test"}}]</script>` +
		`<script crossorigin src="https://a-v2.sndcdn.com/assets/0-aaaa.js"></script>` +
		`<script crossorigin src="https://a-v2.sndcdn.com/assets/55-bbbb.js"></script>`
	if m := reSoundCloudGeo.FindStringSubmatch(page); m == nil || m[1] != "XA" {
		t.Errorf("region = %v, want XA", m)
	}
	if got := len(reSoundCloudScript.FindAllStringSubmatch(page, -1)); got != 2 {
		t.Errorf("found %d bundles, want 2", got)
	}
	bundle := `web_errors_host:"https://web-errors.soundcloud.com",client_application_id:46941,` +
		`client_id:"0123456789abcdefABCDEF0123456789",client_is_expiring:!1`
	if m := reSoundCloudClient.FindStringSubmatch(bundle); m == nil || m[1] != "0123456789abcdefABCDEF0123456789" {
		t.Errorf("client_id = %v", m)
	}
}

func TestClassifyKinoWatch(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   State
	}{
		{"login form", 200, `<form id="login-form" action="/user/login" method="post">`, StateAvailable},
		// A network's block page answers 200 too; only kinopub's own form
		// counts as reaching the site.
		{"page in its place", 200, `<html><body>Доступ ограничен</body></html>`, StateError},
		{"server error", 502, "Bad Gateway", StateError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyKinoWatch(c.status, c.body); got.State != c.want {
				t.Errorf("state = %v, want %v (detail %q)", got.State, c.want, got.Detail)
			}
		})
	}
}

func TestKinoWatchTechResult(t *testing.T) {
	hosts := []kinoWatchHost{
		{"https://api.service-kp.com/v1/user", 401},
		{"https://media.service-kp.com/", 204},
	}
	if got := kinoWatchTechResult(hosts, []string{"", ""}); got.State != StateAvailable {
		t.Errorf("all up: state = %v", got.State)
	}
	one := kinoWatchTechResult(hosts, []string{"HTTP 200", ""})
	if one.State != StateRestricted || one.Detail != "1 unreachable: api.service-kp.com (HTTP 200)" {
		t.Errorf("one down: %v %q", one.State, one.Detail)
	}
	if got := kinoWatchTechResult(hosts, []string{"no answer", "no answer"}); got.State != StateBlocked {
		t.Errorf("all down: state = %v", got.State)
	}
}

func TestKinoWatchManifest(t *testing.T) {
	item := `{"status":200,"item":{"id":1,"videos":[{"id":7,"files":[` +
		`{"quality":"1080p","url":{"http":"https://cdn.example/1.mp4","hls":"https://cdn.example/hls/1.m3u8",` +
		`"hls4":"https://cdn.example/hls4/1.m3u8?loc=nl"}}]}]}}`
	if got, _ := kinoWatchManifest(200, []byte(item)); got != "https://cdn.example/hls4/1.m3u8?loc=nl" {
		t.Errorf("stream = %q, want the hls4 one", got)
	}
	cases := []struct {
		name   string
		status int
		body   string
	}{
		// An expired token says nothing about the network.
		{"expired token", 401, `{"status":401,"error":"unauthorized"}`},
		{"no files", 200, `{"status":200,"item":{"id":1,"videos":[{"id":7,"files":[]}]}}`},
		{"not json", 200, "<html>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stream, got := kinoWatchManifest(c.status, []byte(c.body))
			if stream != "" || got.State != StateError {
				t.Errorf("stream %q, state %v, want an error", stream, got.State)
			}
		})
	}
}

func TestClassifyKinoWatchStream(t *testing.T) {
	stream := "https://cdn.example/hls4/1.m3u8?loc=nl"
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1280x720\n720.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2,RESOLUTION=1920x1080\n1080.m3u8\n"

	got := classifyKinoWatchStream(stream, 200, master, nil)
	if got.State != StateAvailable || got.Region != "NL" || got.Detail != "1080p from cdn.example" {
		t.Errorf("served: %v %q %q", got.State, got.Region, got.Detail)
	}
	// The API handing out a stream the CDN then refuses to connect for is
	// the case this check exists for: site and API up, playback dead.
	if got := classifyKinoWatchStream(stream, 0, "", errors.New("reset")); got.State != StateBlocked {
		t.Errorf("no answer: state = %v", got.State)
	}
	if got := classifyKinoWatchStream(stream, 200, "<html>stub</html>", nil); got.State != StateError {
		t.Errorf("not a playlist: state = %v", got.State)
	}
}
