package access

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"

	"github.com/remnawave/geocheck/internal/netx"
)

// The Twitch probes answer two different questions, which is why they are two
// checks rather than one.
//
// The first asks whether the edge will hand this address a playback token at
// all. Twitch states a refusal in the token itself, in plain prose, and the
// sentence worth catching is the proxy one, which the player shows as
//
//	A proxy or unblocker has been detected. This premium content will not be
//	available to you while it is in use. (Error #3)
//
// That is a different verdict from a geographic block. Twitch will agree you
// are in a served country and still refuse the address because it believes the
// traffic arrives through a proxy or unblocker — exactly the disagreement this
// tool exists to surface.
//
// The second asks whether the hosts a session depends on are reachable, since
// one filtered subdomain (the manifest host is the usual one) breaks playback
// while the site itself still loads and looks healthy.

const (
	// twitchClientID is the public web client's ID. It is not a secret: every
	// browser session sends it, which is why an unauthenticated probe can ask
	// the same question the player asks.
	twitchClientID = "kimne78kx3ncx6brgo4mv6wki5h1ko"
	// twitchPlaybackHash identifies the persisted PlaybackAccessToken query.
	// Twitch accepts only persisted queries from this client, so the hash has
	// to be the one the site uses.
	twitchPlaybackHash = "0828119ded1c13477966434e15800ff57ddacf13ba1911c129dc2200705b0712"

	twitchGQL   = "https://gql.twitch.tv/gql"
	twitchUsher = "https://usher.ttvnw.net/api/channel/hls/"

	// twitchChannel is the channel the token is requested for. Twitch's own
	// channel is used because it always exists; a token is issued whether or
	// not it happens to be live, and the refusal reasons are decided by the
	// requesting address rather than by the channel.
	twitchChannel = "twitch"
)

// twitchProxyMarkers are the two halves of the proxy refusal. Either alone is
// enough: the prose has been reworded before, and the error number has outlived
// several wordings.
var twitchProxyMarkers = []string{
	"proxy or unblocker has been detected",
	"(error #3)",
}

const twitchProxyDetail = "proxy or unblocker detected (Error #3)"

func hasTwitchProxyMarker(body string) bool {
	lower := strings.ToLower(body)
	for _, m := range twitchProxyMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// twitchGQLReply is the envelope. The token itself arrives as a JSON document
// inside a JSON string, which is why Value is decoded a second time.
type twitchGQLReply struct {
	Data struct {
		Token *struct {
			Value     string `json:"value"`
			Signature string `json:"signature"`
		} `json:"streamPlaybackAccessToken"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type twitchToken struct {
	Authorization struct {
		Forbidden bool   `json:"forbidden"`
		Reason    string `json:"reason"`
	} `json:"authorization"`
	// GeoblockReason and CIGB are how Twitch says "wrong country", as opposed
	// to "wrong kind of address".
	GeoblockReason string `json:"geoblock_reason"`
	CIGB           bool   `json:"ci_gb"`
}

// classifyTwitchToken turns one gql reply into a verdict. It is separate from
// the request so the decision can be tested against captured responses.
//
// The whole body is scanned for the proxy sentence before anything is decoded,
// because the refusal has appeared in more than one field over time and the
// sentence is the part that has stayed put.
//
// A granted token is returned alongside the verdict so the caller can put the
// same question to the manifest host, which checks the address again after gql
// has already allowed it.
func classifyTwitchToken(status int, body string) (res Result, value, sig string) {
	if hasTwitchProxyMarker(body) {
		return Result{State: StateBlocked, Detail: twitchProxyDetail}, "", ""
	}
	if status < 200 || status >= 300 {
		return Result{State: StateError, Detail: "unexpected HTTP " + itoa(status)}, "", ""
	}

	var reply twitchGQLReply
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		return Result{State: StateError, Detail: "unreadable token response", Err: err}, "", ""
	}
	if len(reply.Errors) > 0 {
		return Result{State: StateError, Detail: "gql error: " + reply.Errors[0].Message}, "", ""
	}
	if reply.Data.Token == nil {
		return Result{State: StateError, Detail: "no playback token was issued"}, "", ""
	}

	var tok twitchToken
	if err := json.Unmarshal([]byte(reply.Data.Token.Value), &tok); err != nil {
		return Result{State: StateError, Detail: "unreadable token payload", Err: err}, "", ""
	}

	switch {
	case tok.Authorization.Forbidden:
		detail := tok.Authorization.Reason
		if detail == "" {
			detail = "playback forbidden"
		}
		return Result{State: StateBlocked, Detail: detail}, "", ""
	case tok.CIGB || tok.GeoblockReason != "":
		detail := "geoblocked"
		if tok.GeoblockReason != "" {
			detail = "geoblocked: " + tok.GeoblockReason
		}
		return Result{State: StateBlocked, Detail: detail}, "", ""
	}

	return Result{State: StateAvailable}, reply.Data.Token.Value, reply.Data.Token.Signature
}

func twitchAccess() Check {
	return Check{
		ID: "twitch_access", Name: "Twitch",
		Run: func(ctx context.Context, env Env) Result {
			query := `{"operationName":"PlaybackAccessToken","variables":` +
				`{"isLive":true,"login":"` + twitchChannel + `","isVod":false,` +
				`"vodID":"","playerType":"site"},"extensions":{"persistedQuery":` +
				`{"version":1,"sha256Hash":"` + twitchPlaybackHash + `"}}}`

			resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
				Method:    "POST",
				URL:       twitchGQL,
				UserAgent: browserUA,
				JSON:      query,
				Headers: map[string]string{
					"Client-ID": twitchClientID,
					"Origin":    "https://www.twitch.tv",
					"Referer":   "https://www.twitch.tv/",
				},
			})
			if err != nil {
				return Result{State: StateError, Detail: "request failed", Err: err}
			}

			res, value, sig := classifyTwitchToken(resp.Status, resp.Text())
			if value == "" {
				return res
			}
			if manifest := twitchManifest(ctx, env, value, sig); manifest != nil {
				return *manifest
			}
			return res
		},
	}
}

// twitchManifest puts the question to the manifest host as well, and returns
// nil unless it has something to add.
//
// A token is not on its own enough to reach a manifest — the channel also has
// to be live — so anything short of an outright refusal is not evidence either
// way, and is deliberately left unreported rather than turned into a verdict.
func twitchManifest(ctx context.Context, env Env, value, sig string) *Result {
	q := url.Values{
		"allow_source": {"true"},
		"fast_bread":   {"true"},
		"token":        {value},
		"sig":          {sig},
	}
	resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
		URL:       twitchUsher + twitchChannel + ".m3u8?" + q.Encode(),
		UserAgent: browserUA,
	})
	if err != nil {
		return nil
	}
	if hasTwitchProxyMarker(resp.Text()) {
		return &Result{State: StateBlocked, Detail: twitchProxyDetail}
	}
	return nil
}

// twitchHosts are the origins one Twitch session actually depends on. They are
// listed separately because they fail separately: filtering usually lands on
// one of them — most often the manifest host — while the rest stay up, so the
// site loads and only playback breaks.
var twitchHosts = []string{
	"www.twitch.tv",
	"m.twitch.tv",
	"gql.twitch.tv",
	"api.twitch.tv",
	"id.twitch.tv",
	"passport.twitch.tv",
	"player.twitch.tv",
	"clips.twitch.tv",
	"assets.twitch.tv",
	"static-cdn.jtvnw.net",
	"usher.ttvnw.net",
	"vod-secure.twitch.tv",
}

// twitchHostUp reports whether a host answered at all. The status is not read
// as a verdict: these hosts answer 403, 404 and 405 to a bare request in normal
// operation, and any of those still proves the host was reached. Only a
// transport failure or a 5xx says otherwise.
func twitchHostUp(status int, err error) bool {
	return err == nil && status < 500
}

func twitchEndpoints() Check {
	return Check{
		ID: "twitch_endpoints", Name: "Twitch endpoints",
		Run: func(ctx context.Context, env Env) Result {
			down := make([]string, len(twitchHosts))
			var wg sync.WaitGroup
			for i, host := range twitchHosts {
				wg.Add(1)
				go func() {
					defer wg.Done()
					resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
						URL:       "https://" + host + "/",
						UserAgent: browserUA,
						HeadOnly:  true,
					})
					status := 0
					if resp != nil {
						status = resp.Status
					}
					if !twitchHostUp(status, err) {
						down[i] = host
					}
				}()
			}
			wg.Wait()

			failed := make([]string, 0, len(down))
			for _, h := range down {
				if h != "" {
					failed = append(failed, h)
				}
			}
			return twitchEndpointResult(len(twitchHosts), failed)
		},
	}
}

func twitchEndpointResult(total int, failed []string) Result {
	switch {
	case len(failed) == 0:
		return Result{
			State:  StateAvailable,
			Detail: itoa(total) + "/" + itoa(total) + " hosts reachable",
		}
	case len(failed) == total:
		return Result{State: StateBlocked, Detail: "no Twitch host answered"}
	}
	return Result{
		State:  StateRestricted,
		Detail: itoa(len(failed)) + " unreachable: " + strings.Join(failed, ", "),
	}
}
