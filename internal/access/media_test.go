package access

import (
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
