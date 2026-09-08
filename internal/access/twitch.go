package access

import (
	"context"
	"encoding/json"
	"net/netip"
	"net/url"
	"regexp"
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

// twitchAnonymizerReason reports whether a geoblock_reason is about the kind of
// address rather than where it is.
//
// The field name invites the wrong reading. "anonymizer_blocked" arrives in it,
// and it is not a country verdict at all: it is Twitch saying the address looks
// like hosting or proxy space, which is the same finding the player renders as
// the proxy sentence. Reporting it as a geoblock would be actively misleading —
// it sends someone off to change their exit country when the country was never
// the problem, and a correct country is exactly when this turns up.
func twitchAnonymizerReason(reason string) bool {
	return strings.Contains(strings.ToLower(reason), "anonymizer")
}

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
	// GeoblockReason and CIGB carry the refusals Twitch decides before it
	// looks at entitlements. The name is misleading: not every value is
	// geographic — see twitchAnonymizerReason.
	GeoblockReason string `json:"geoblock_reason"`
	CIGB           bool   `json:"ci_gb"`
	// UserIP is the address gql issued the token to. It is baked into the
	// signed token and checked again when the manifest is fetched, which is
	// what makes it worth reading: see twitchSplitPath.
	UserIP string `json:"user_ip"`
	// MaxResolution and MaxResolutionReasons are the quality ceiling and, for
	// each tier above it, why it is withheld. They are in the same token as
	// the availability verdict, which is the whole reason to read them: the
	// choice between routing Twitch through a tunnel and leaving it direct
	// trades one against the other, and both sides can be measured at once.
	MaxResolution        string              `json:"maximum_resolution"`
	MaxResolutionReasons map[string][]string `json:"maximum_resolution_reasons"`
}

// twitchTiers is Twitch's quality ladder, lowest first. Only the tiers seen in
// real tokens are named; anything else is reported as Twitch spells it rather
// than guessed at.
var twitchTiers = []struct{ Tier, Name string }{
	{"FULL_HD", "1080p"},
	{"QUAD_HD", "1440p"},
	{"ULTRA_HD", "2160p"},
}

func twitchTierName(tier string) string {
	for _, t := range twitchTiers {
		if t.Tier == tier {
			return t.Name
		}
	}
	return tier
}

// twitchQuality describes the ceiling and the first tier above it that is
// withheld, with the reason Twitch gives.
//
// The reason code is passed through verbatim. It is self-describing
// (AUTHZ_NOT_LOGGED_IN), and inventing friendlier wording would mean guessing
// at codes not yet seen — including whichever one a region cap uses, which is
// exactly the case this is here to answer.
func twitchQuality(tok twitchToken) string {
	if tok.MaxResolution == "" {
		return ""
	}
	out := "max " + twitchTierName(tok.MaxResolution)

	// The lowest withheld tier is the informative one: it is the next thing
	// that would be gained, and its reason says what is standing in the way.
	best, bestRank := "", len(twitchTiers)+1
	for tier := range tok.MaxResolutionReasons {
		rank := len(twitchTiers)
		for i, t := range twitchTiers {
			if t.Tier == tier {
				rank = i
				break
			}
		}
		if rank < bestRank || (rank == bestRank && tier < best) {
			best, bestRank = tier, rank
		}
	}
	// Every reason, not just the first. A tier can be withheld for more than
	// one, and which ones they are decides whether anything can be done about
	// it: AUTHZ_NOT_LOGGED_IN alone is fixed by signing in, AUTHZ_GEO alongside
	// it means signing in changes nothing.
	if reasons := tok.MaxResolutionReasons[best]; len(reasons) > 0 {
		out += "; " + twitchTierName(best) + ": " + strings.Join(reasons, ", ")
	}
	return out
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
// has already allowed it, and can compare the address gql issued the token to
// against the one the session actually exits from.
func classifyTwitchToken(status int, body string) (res Result, tok twitchToken, value, sig string) {
	// Refusals hand the decoded token back too: the address it names is what
	// the verdict was actually about, and the caller reports it.
	fail := func(r Result) (Result, twitchToken, string, string) {
		return r, tok, "", ""
	}

	if hasTwitchProxyMarker(body) {
		return fail(Result{State: StateBlocked, Detail: twitchProxyDetail})
	}
	if status < 200 || status >= 300 {
		return fail(Result{State: StateError, Detail: "unexpected HTTP " + itoa(status)})
	}

	var reply twitchGQLReply
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		return fail(Result{State: StateError, Detail: "unreadable token response", Err: err})
	}
	if len(reply.Errors) > 0 {
		return fail(Result{State: StateError, Detail: "gql error: " + reply.Errors[0].Message})
	}
	if reply.Data.Token == nil {
		return fail(Result{State: StateError, Detail: "no playback token was issued"})
	}

	if err := json.Unmarshal([]byte(reply.Data.Token.Value), &tok); err != nil {
		return fail(Result{State: StateError, Detail: "unreadable token payload", Err: err})
	}

	switch {
	case tok.Authorization.Forbidden:
		detail := tok.Authorization.Reason
		if detail == "" {
			detail = "playback forbidden"
		}
		return fail(Result{State: StateBlocked, Detail: detail})
	case twitchAnonymizerReason(tok.GeoblockReason):
		// Same finding as the sentence above, reported in the same words: this
		// is the machine-readable form of the message the player shows.
		return fail(Result{State: StateBlocked, Detail: twitchProxyDetail})
	case tok.CIGB || tok.GeoblockReason != "":
		detail := "geoblocked"
		if tok.GeoblockReason != "" {
			detail = "geoblocked: " + tok.GeoblockReason
		}
		return fail(Result{State: StateBlocked, Detail: detail})
	}

	return Result{State: StateAvailable, Detail: twitchQuality(tok)},
		tok, reply.Data.Token.Value, reply.Data.Token.Signature
}

// twitchSplitPath compares the address Twitch issued the token to against the
// address this session exits from, and reports the disagreement.
//
// The comparison is the useful one because Twitch binds the token to the
// address that asked for it and checks it again when the manifest is fetched.
// A routing rule that sends gql one way and the rest of the session another —
// the shape every published workaround for this error has in common, whether it
// routes Twitch direct and the manifest host through a tunnel or the reverse —
// makes those two addresses differ, and the refusal that follows is worded as a
// proxy detection.
//
// So this is reported before Twitch has refused anything: the configuration
// that produces the error is visible in the token itself, and saying so is more
// use than waiting for the error to appear.
func twitchSplitPath(tok twitchToken, public netip.Addr) *Result {
	seen, ok := twitchSeenElsewhere(tok, public)
	if !ok {
		return nil
	}
	return &Result{
		State:  StateRestricted,
		Detail: "token issued to " + seen + ", session exits " + public.String(),
	}
}

// twitchSeenElsewhere returns the address gql issued the token to, and whether
// it is a different one from where the session exits.
func twitchSeenElsewhere(tok twitchToken, public netip.Addr) (string, bool) {
	if tok.UserIP == "" || !public.IsValid() {
		return "", false
	}
	seen, err := netip.ParseAddr(tok.UserIP)
	if err != nil {
		return "", false
	}
	if seen.Unmap() == public.Unmap() {
		return "", false
	}
	return seen.String(), true
}

// twitchNoteAddress names the address Twitch judged, when that is not the one
// the session exits from.
//
// On a refusal this is the single most useful fact in the result. Route
// Twitch's domains separately from everything else — which every workaround
// for this error tells people to do — and the verdict is about an address the
// report otherwise never shows, so "blocked" gives no way to tell which exit
// was rejected and no way to know which one to change.
func twitchNoteAddress(res Result, tok twitchToken, public netip.Addr) Result {
	seen, ok := twitchSeenElsewhere(tok, public)
	if !ok || res.Detail == "" {
		return res
	}
	res.Detail += "; Twitch saw " + seen
	return res
}

// twitchAccess probes the default channel, which answers the general question:
// will Twitch issue this address a token at all.
func twitchAccess() Check {
	return twitchAccessFor(twitchChannel, "twitch_access", "Twitch")
}

// twitchAccessFor probes one named channel.
//
// A separate channel is worth probing because the premium refusal is a
// property of the content, not of the account: a channel with no restricted
// content is served to addresses that a restricted one would refuse, so the
// default channel can come back clean while the channel someone actually wants
// to watch does not. Pointing this at that channel is the only way to ask the
// question that matters to them.
func twitchAccessFor(channel, id, name string) Check {
	return Check{
		ID: id, Name: name,
		Run: func(ctx context.Context, env Env) Result {
			query := `{"operationName":"PlaybackAccessToken","variables":` +
				`{"isLive":true,"login":"` + channel + `","isVod":false,` +
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

			res, tok, value, sig := classifyTwitchToken(resp.Status, resp.Text())
			if value == "" {
				return twitchNoteAddress(res, tok, env.PublicIP)
			}
			// An outright refusal from the manifest host is hard evidence and
			// outranks the configuration warning below.
			manifest, offered := twitchManifest(ctx, env, channel, value, sig)
			if manifest != nil {
				return *manifest
			}
			if split := twitchSplitPath(tok, env.PublicIP); split != nil {
				return *split
			}
			// What is actually served leads, because it is the ground truth;
			// the ceiling behind it says whether a lower number is Twitch's
			// doing or the channel's own.
			if offered != "" {
				res.Detail = strings.TrimSuffix("offered "+offered+"; "+res.Detail, "; ")
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
// reStreamInf reads a rendition off a master playlist entry. Frame rate is
// optional: not every entry carries one.
var (
	reStreamRes = regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
	reStreamFPS = regexp.MustCompile(`FRAME-RATE=([0-9.]+)`)
)

// twitchTopRendition names the best rendition a master playlist actually
// offers.
//
// This is the number that matters, and it is not the one in the token. The
// token carries a ceiling — permission — while the playlist carries what is
// really being served, and the two disagree for two very different reasons:
// Twitch capping the region, or the channel simply not broadcasting higher.
// Without this, a 720p player next to a token saying 1080p looks like a
// regional cap even when the streamer's own source is the limit.
func twitchTopRendition(playlist string) string {
	bestH, bestFPS := 0, 0.0
	for _, line := range strings.Split(playlist, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		m := reStreamRes.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		h := 0
		for _, c := range m[2] {
			h = h*10 + int(c-'0')
		}

		fps := 0.0
		if f := reStreamFPS.FindStringSubmatch(line); f != nil {
			whole, frac, _ := strings.Cut(f[1], ".")
			for _, c := range whole {
				fps = fps*10 + float64(c-'0')
			}
			if frac != "" && frac[0] >= '5' {
				fps++
			}
		}

		if h > bestH || (h == bestH && fps > bestFPS) {
			bestH, bestFPS = h, fps
		}
	}
	if bestH == 0 {
		return ""
	}

	out := itoa(bestH) + "p"
	// Twitch labels only the high frame rate ladders, and so does this: 720p
	// and 720p60 are different products, 30 and 25 are not worth the noise.
	if bestFPS >= 50 {
		out += itoa(int(bestFPS))
	}
	return out
}

func twitchManifest(ctx context.Context, env Env, channel, value, sig string) (*Result, string) {
	q := url.Values{
		"allow_source": {"true"},
		"fast_bread":   {"true"},
		"token":        {value},
		"sig":          {sig},
	}
	resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
		URL:       twitchUsher + channel + ".m3u8?" + q.Encode(),
		UserAgent: browserUA,
	})
	if err != nil {
		return nil, ""
	}
	body := resp.Text()
	if hasTwitchProxyMarker(body) {
		return &Result{State: StateBlocked, Detail: twitchProxyDetail}, ""
	}
	// Empty unless the channel is live: an offline channel has no playlist,
	// which is absence of evidence rather than a cap, and is reported as
	// nothing rather than as a number.
	return nil, twitchTopRendition(body)
}

// twitchHost is one origin a Twitch session depends on. They are listed
// individually because they fail individually: filtering usually lands on one
// of them — most often the manifest host — while the rest stay up, so the site
// loads and only playback breaks.
type twitchHost struct {
	Host string
	// Ads marks the ad and telemetry endpoints. These fail for a different
	// reason from the rest and mean something different when they do: nothing
	// stops working, but a client that cannot reach them is a client that
	// looks like it is suppressing ads, which is the other half of what Twitch
	// calls an unblocker. Blackholing them — via an ad-blocking rule, or by
	// routing an ads list to a reject outbound — is the second published cause
	// of this error, and unlike a broken dependency it leaves the session
	// working right up until playback is refused.
	Ads bool
}

var twitchHosts = []twitchHost{
	{Host: "www.twitch.tv"},
	{Host: "m.twitch.tv"},
	{Host: "gql.twitch.tv"},
	{Host: "api.twitch.tv"},
	{Host: "id.twitch.tv"},
	{Host: "passport.twitch.tv"},
	{Host: "player.twitch.tv"},
	{Host: "clips.twitch.tv"},
	{Host: "assets.twitch.tv"},
	{Host: "static-cdn.jtvnw.net"},
	{Host: "static.twitchcdn.net"},
	{Host: "extension-files.twitch.tv"},
	{Host: "pubsub-edge.twitch.tv"},
	{Host: "irc-ws.chat.twitch.tv"},
	{Host: "usher.ttvnw.net"},
	{Host: "vod-secure.twitch.tv"},
	{Host: "edge.ads.twitch.tv", Ads: true},
	{Host: "spade.twitch.tv", Ads: true},
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
			down := make([]bool, len(twitchHosts))
			var wg sync.WaitGroup
			for i, h := range twitchHosts {
				wg.Add(1)
				go func() {
					defer wg.Done()
					resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
						URL:       "https://" + h.Host + "/",
						UserAgent: browserUA,
						HeadOnly:  true,
					})
					status := 0
					if resp != nil {
						status = resp.Status
					}
					down[i] = !twitchHostUp(status, err)
				}()
			}
			wg.Wait()

			return twitchEndpointResult(twitchHosts, down)
		},
	}
}

// twitchEndpointResult reads the sweep. The ad endpoints are separated out
// because losing only those is a different finding from losing a dependency:
// everything still works, and the report would otherwise say "restricted" over
// two hosts nothing visibly needs, when what it has actually found is the
// configuration that gets playback refused as an unblocker.
func twitchEndpointResult(hosts []twitchHost, down []bool) Result {
	failed := make([]string, 0, len(hosts))
	ads := make([]string, 0, len(hosts))
	for i, h := range hosts {
		if !down[i] {
			continue
		}
		if h.Ads {
			ads = append(ads, h.Host)
			continue
		}
		failed = append(failed, h.Host)
	}

	switch {
	case len(failed) == 0 && len(ads) == 0:
		return Result{
			State:  StateAvailable,
			Detail: itoa(len(hosts)) + "/" + itoa(len(hosts)) + " hosts reachable",
		}
	case len(failed)+len(ads) == len(hosts):
		return Result{State: StateBlocked, Detail: "no Twitch host answered"}
	case len(failed) == 0:
		return Result{
			State:  StateRestricted,
			Detail: "ads blackholed, reads as an unblocker: " + strings.Join(ads, ", "),
		}
	}
	return Result{
		State:  StateRestricted,
		Detail: itoa(len(failed)) + " unreachable: " + strings.Join(failed, ", "),
	}
}

// reTwitchLogin is Twitch's own rule for a channel name: letters, digits and
// underscores, 4 to 25 of them.
var reTwitchLogin = regexp.MustCompile(`^[A-Za-z0-9_]{4,25}$`)

// TwitchChannel reads a channel login out of whatever the user pasted, which
// in practice is either the name or the address bar. It returns the login in
// the lower case Twitch uses, and false if the input names no channel.
//
// Accepting the URL matters more than it looks: the channel someone wants
// tested is the one they are looking at, and asking them to retype the last
// path segment of it is the kind of small refusal that gets a flag left unused.
func TwitchChannel(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}

	// Tolerate a full URL, a scheme-less one, and any trailing path or query
	// Twitch hangs off a channel page.
	if i := strings.Index(s, "twitch.tv/"); i >= 0 {
		s = s[i+len("twitch.tv/"):]
	}
	s = strings.TrimPrefix(s, "/")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}

	if !reTwitchLogin.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}
