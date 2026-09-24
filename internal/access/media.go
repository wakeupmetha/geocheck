package access

import (
	"context"
	"encoding/json"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/remnawave/geocheck/internal/jsonx"
	"github.com/remnawave/geocheck/internal/netx"
)

// The media probes cover services whose refusal is about licensing rather than
// sanctions: Kinopoisk and VK Video hold rights for Russia and withhold content
// from addresses outside it, or from ones they take for a VPN, while SoundCloud
// is the opposite case — served abroad and blocked from inside Russia. Each
// asks the question the service's own client asks, so the answer is the one a
// person pressing play would get.

// kinopoiskQuery is the document Kinopoisk's web client sends to decide whether
// to show its "VPN is on, or not the whole catalogue is available in your
// country" banner. The gateway only runs documents it already knows, so this
// has to match the client's text exactly, __typename selections included.
const kinopoiskQuery = `{"operationName":"OttData","variables":{},"query":` +
	`"query OttData { ott { contentAvailability { geo { availabilityStatus __typename } __typename } __typename } }"}`

// classifyKinopoisk reads the verdict out of the gateway's reply. The web
// client treats a missing status as available; a check cannot, because a
// missing status is just as consistent with a changed schema.
func classifyKinopoisk(status int, body string) Result {
	b := []byte(body)
	switch jsonx.String(b, "data.ott.contentAvailability.geo.availabilityStatus") {
	case "AVAILABLE":
		return Result{State: StateAvailable}
	case "RESTRICTED":
		return Result{
			State:  StateRestricted,
			Detail: "VPN suspected or region outside the licence",
		}
	}
	if msg := jsonx.String(b, "errors.0.message"); msg != "" {
		return Result{State: StateError, Detail: "gql error: " + msg}
	}
	if status < 200 || status >= 300 {
		return Result{State: StateError, Detail: "unexpected HTTP " + itoa(status)}
	}
	return Result{State: StateError, Detail: "no availability status in the reply"}
}

func kinopoisk() Check {
	return Check{
		ID: "kinopoisk_access", Name: "Kinopoisk",
		Run: func(ctx context.Context, env Env) Result {
			// Without the service id the gateway answers 404.
			resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
				Method:    "POST",
				URL:       "https://graphql.kinopoisk.ru/graphql/?operationName=OttData",
				UserAgent: browserUA,
				JSON:      kinopoiskQuery,
				Headers:   map[string]string{"Service-Id": "25"},
			})
			if err != nil {
				return Result{State: StateError, Detail: "request failed", Err: err}
			}
			return classifyKinopoisk(resp.Status, resp.Text())
		},
	}
}

// vkVideoClientID and vkVideoClientSecret are the public credentials the
// vkvideo.ru web client ships with, used to fetch the anonymous token it
// browses with before anyone signs in.
const (
	vkVideoClientID     = "52461373"
	vkVideoClientSecret = "o557NLIkAErNhakXrQ7A"
)

// classifyVKVideo judges the front-page catalogue by how much of it would
// actually play. VK withholds licensed videos per title rather than refusing
// the site, so the page loads either way; what changes is whether a video
// carries stream URLs. Counting those across the catalogue separates a full
// offer from a partial one without depending on any single title staying up.
func classifyVKVideo(status int, body string, public netip.Addr) Result {
	// Why is left untyped: a mismatch would fail the whole decode over a field
	// that only decorates the verdict.
	var reply struct {
		Response struct {
			Videos []struct {
				Files map[string]any `json:"files"`
				Why   any            `json:"content_restricted_message"`
			} `json:"videos"`
		} `json:"response"`
		Error struct {
			Msg string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		if status < 200 || status >= 300 {
			return Result{State: StateError, Detail: "unexpected HTTP " + itoa(status)}
		}
		return Result{State: StateError, Detail: "unreadable catalogue response", Err: err}
	}
	if reply.Error.Msg != "" {
		return Result{State: StateError, Detail: "api error: " + reply.Error.Msg}
	}

	videos := reply.Response.Videos
	if len(videos) == 0 {
		return Result{State: StateError, Detail: "the catalogue listed no videos"}
	}
	playable, seen, why := 0, "", ""
	for _, v := range videos {
		if len(v.Files) == 0 {
			if s, ok := v.Why.(string); ok && why == "" {
				why = s
			}
			continue
		}
		playable++
		if seen == "" {
			seen = vkSourceIP(v.Files)
		}
	}

	res := Result{
		State:  StateAvailable,
		Detail: itoa(playable) + "/" + itoa(len(videos)) + " videos playable",
	}
	switch {
	case playable == 0:
		res.State = StateBlocked
	case playable < len(videos):
		res.State = StateRestricted
	}
	if why != "" {
		res.Detail += "; " + why
	}
	// Stream URLs are signed for the address that asked for them, which makes
	// them the one place VK says who it served. Russian services are the ones
	// most often routed out a separate exit, so the address is worth naming.
	if addr, ok := seenElsewhere(seen, public); ok {
		res.Detail += "; VK saw " + addr
	}
	return res
}

// vkSourceIP reads the address a stream URL was signed for.
func vkSourceIP(files map[string]any) string {
	for _, f := range files {
		s, _ := f.(string)
		u, err := url.Parse(s)
		if err != nil {
			continue
		}
		if ip := u.Query().Get("srcIp"); ip != "" {
			return ip
		}
	}
	return ""
}

func vkVideo() Check {
	return Check{
		ID: "vkvideo_access", Name: "VK Video",
		Run: func(ctx context.Context, env Env) Result {
			tok, err := env.Stack.Do(ctx, env.Family, netx.Request{
				Method:    "POST",
				URL:       "https://login.vk.ru/?act=get_anonym_token",
				UserAgent: browserUA,
				Form: url.Values{
					"client_id":     {vkVideoClientID},
					"client_secret": {vkVideoClientSecret},
				},
			})
			if err != nil {
				return Result{State: StateError, Detail: "request failed", Err: err}
			}
			token := jsonx.String(tok.Body, "data.access_token")
			if token == "" {
				return Result{
					State:  StateError,
					Detail: "no anonymous token (HTTP " + itoa(tok.Status) + "), so the catalogue was never asked",
				}
			}

			// Without need_blocks the catalogue returns its sections only.
			resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
				Method:    "POST",
				URL:       "https://api.vkvideo.ru/method/catalog.getVideo?v=5.289&client_id=" + vkVideoClientID,
				UserAgent: browserUA,
				Form: url.Values{
					"access_token": {token},
					"need_blocks":  {"1"},
					"owner_id":     {"0"},
					"url":          {"https://vkvideo.ru/"},
				},
			})
			if err != nil {
				return Result{State: StateError, Detail: "request failed", Err: err}
			}
			return classifyVKVideo(resp.Status, resp.Text(), env.PublicIP)
		},
	}
}

// soundCloudTrack is Forss' "Flickermood", the long-standing example track in
// SoundCloud's API documentation. It carries no label restrictions — its policy
// stays ALLOW even where label catalogues are withheld — so a refusal to stream
// it is about the address, not the track.
const soundCloudTrack = "293"

var (
	reSoundCloudGeo    = regexp.MustCompile(`"hydratable":"geoip","data":\{"country_code":"([A-Z]{2})"`)
	reSoundCloudScript = regexp.MustCompile(`<script[^>]+src="(https://a-v2\.sndcdn\.com/assets/[^"]+\.js)"`)
	reSoundCloudClient = regexp.MustCompile(`client_id:"([0-9A-Za-z]{32})"`)
)

// classifySoundCloudTrack returns the stream to request next, or the verdict
// when the track reply already settles it.
func classifySoundCloudTrack(status int, body string) (string, Result) {
	if status == 401 {
		return "", Result{State: StateError, Detail: "the API rejected the client's own client_id"}
	}
	if status < 200 || status >= 300 {
		return "", Result{State: StateError, Detail: "unexpected HTTP " + itoa(status) + " from the API"}
	}

	var track struct {
		Policy string `json:"policy"`
		Media  struct {
			Transcodings []struct {
				URL     string `json:"url"`
				Snipped bool   `json:"snipped"`
			} `json:"transcodings"`
		} `json:"media"`
	}
	if err := json.Unmarshal([]byte(body), &track); err != nil {
		return "", Result{State: StateError, Detail: "unreadable track response", Err: err}
	}
	// SoundCloud's own words for these: BLOCK is "not available in your
	// country", SNIP is a 30-second preview.
	switch track.Policy {
	case "BLOCK":
		return "", Result{State: StateBlocked, Detail: "not available in this country"}
	case "SNIP":
		return "", Result{State: StateRestricted, Detail: "30-second previews only"}
	}
	for _, t := range track.Media.Transcodings {
		if !t.Snipped && t.URL != "" {
			return t.URL, Result{}
		}
	}
	return "", Result{State: StateError, Detail: "no full-length stream was offered"}
}

// soundCloudClientID reads the public client_id out of the web client's
// bundles. SoundCloud rotates it, so it is looked up on every run rather than
// pinned; the bundle carrying it has been the last on the page, so the search
// runs from the end.
func soundCloudClientID(ctx context.Context, env Env, page string) string {
	scripts := reSoundCloudScript.FindAllStringSubmatch(page, -1)
	for i := len(scripts) - 1; i >= 0; i-- {
		resp, err := env.Stack.Do(ctx, env.Family, netx.Request{URL: scripts[i][1], UserAgent: browserUA})
		if err != nil {
			continue
		}
		if m := reSoundCloudClient.FindStringSubmatch(resp.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}

// soundCloud follows the path the player takes — page, track, stream — and only
// reports available once a stream URL is actually issued, since the page loads
// in places where playback does not.
func soundCloud() Check {
	return Check{
		ID: "soundcloud_access", Name: "SoundCloud",
		Run: func(ctx context.Context, env Env) Result {
			home, err := env.Stack.Do(ctx, env.Family, netx.Request{
				URL:       "https://soundcloud.com/",
				UserAgent: browserUA,
			})
			if err != nil {
				return Result{State: StateError, Detail: "request failed", Err: err}
			}
			page := home.Text()
			if isChallenge(home.Status, strings.ToLower(page)) {
				return Result{
					State:  StateError,
					Detail: "Cloudflare challenged the request, so availability was never tested",
				}
			}
			region := ""
			if m := reSoundCloudGeo.FindStringSubmatch(page); m != nil {
				region = m[1]
			}

			clientID := soundCloudClientID(ctx, env, page)
			if clientID == "" {
				return Result{
					State: StateError, Region: region,
					Detail: "no client_id in the web client, so playback was never tested",
				}
			}

			resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
				URL:       "https://api-v2.soundcloud.com/tracks/" + soundCloudTrack + "?client_id=" + clientID,
				UserAgent: browserUA,
			})
			if err != nil {
				return Result{State: StateError, Region: region, Detail: "request failed", Err: err}
			}
			stream, res := classifySoundCloudTrack(resp.Status, resp.Text())
			if stream == "" {
				res.Region = region
				return res
			}

			resp, err = env.Stack.Do(ctx, env.Family, netx.Request{
				URL:       stream + "?client_id=" + clientID,
				UserAgent: browserUA,
			})
			if err != nil {
				return Result{State: StateError, Region: region, Detail: "request failed", Err: err}
			}
			if jsonx.String(resp.Body, "url") == "" {
				return Result{
					State: StateError, Region: region,
					Detail: "no stream URL was issued (HTTP " + itoa(resp.Status) + ")",
				}
			}
			return Result{State: StateAvailable, Region: region}
		},
	}
}

// kino.watch is kinopub's current address. Unlike the services above it has no
// regional licence to enforce: it is blocked from inside Russia by the network,
// not refused by the service, and blocked one host at a time. The site, the API
// its apps talk to and the CDN the player streams from fail independently, so
// each is its own check.

// classifyKinoWatch judges the front page. Anonymous visitors are sent to the
// login form, so reaching the form is the evidence the site was reached; any
// other page in its place is most likely a block page served on its behalf.
func classifyKinoWatch(status int, body string) Result {
	if isChallenge(status, strings.ToLower(body)) {
		return Result{
			State:  StateError,
			Detail: "Cloudflare challenged the request, so availability was never tested",
		}
	}
	if strings.Contains(body, `id="login-form"`) {
		return Result{State: StateAvailable}
	}
	if status >= 200 && status < 400 {
		return Result{
			State:  StateError,
			Detail: "HTTP " + itoa(status) + " without the login form; a block page in its place?",
		}
	}
	return Result{State: StateError, Detail: "unexpected HTTP " + itoa(status)}
}

func kinoWatch() Check {
	return Check{
		ID: "kinowatch_access", Name: "kino.watch",
		Run: func(ctx context.Context, env Env) Result {
			resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
				URL:       "https://kino.watch/",
				UserAgent: browserUA,
			})
			if err != nil {
				return Result{State: StateError, Detail: "request failed", Err: err}
			}
			return classifyKinoWatch(resp.Status, resp.Text())
		},
	}
}

// kinoWatchHost is one origin of kinopub's technical zone, with the status its
// own nginx gives an anonymous request. Matching that status, rather than
// accepting any answer, is what tells the real server from a block page.
type kinoWatchHost struct {
	URL  string
	Want int
}

// kinoWatchTech is the API the apps use, the mirrors they fall back to when it
// is blocked, and the media and static hosts behind the site.
var kinoWatchTech = []kinoWatchHost{
	{"https://api.service-kp.com/v1/user", 401},
	{"https://api.srvkp.com/v1/user", 401},
	{"https://api.alador.space/v1/user", 401},
	{"https://cdn-service.space/api/v1/user", 401},
	{"https://cdn-service.online/api/v1/user", 401},
	{"https://media.service-kp.com/", 204},
	{"https://m.staticpop.net/", 204},
}

// kinoWatchTechResult reads the sweep. why holds, per host, "" when it answered
// as itself and otherwise what came back instead.
func kinoWatchTechResult(hosts []kinoWatchHost, why []string) Result {
	failed := make([]string, 0, len(hosts))
	for i, h := range hosts {
		if why[i] == "" {
			continue
		}
		u, _ := url.Parse(h.URL)
		failed = append(failed, u.Host+" ("+why[i]+")")
	}
	switch len(failed) {
	case 0:
		return Result{
			State:  StateAvailable,
			Detail: itoa(len(hosts)) + "/" + itoa(len(hosts)) + " hosts reachable",
		}
	case len(hosts):
		return Result{State: StateBlocked, Detail: "no kinopub host answered"}
	}
	return Result{
		State:  StateRestricted,
		Detail: itoa(len(failed)) + " unreachable: " + strings.Join(failed, ", "),
	}
}

func kinoWatchTechZone() Check {
	return Check{
		ID: "kinowatch_tech", Name: "kino.watch tech zone",
		Run: func(ctx context.Context, env Env) Result {
			why := make([]string, len(kinoWatchTech))
			var wg sync.WaitGroup
			for i, h := range kinoWatchTech {
				wg.Add(1)
				go func() {
					defer wg.Done()
					resp, err := env.Stack.Do(ctx, env.Family, netx.Request{
						URL:       h.URL,
						UserAgent: browserUA,
					})
					switch {
					case err != nil:
						why[i] = "no answer"
					case resp.Status != h.Want:
						why[i] = "HTTP " + itoa(resp.Status)
					}
				}()
			}
			wg.Wait()
			return kinoWatchTechResult(kinoWatchTech, why)
		},
	}
}

const kinoWatchAPI = "https://api.service-kp.com/v1/"

// kinoWatchAPIError says why the API refused a request, or "" if it did not.
func kinoWatchAPIError(status int) string {
	switch {
	case status == 401:
		return "the API rejected the token; it may have expired"
	case status < 200 || status >= 300:
		return "unexpected HTTP " + itoa(status) + " from the API"
	}
	return ""
}

// kinoWatchManifest picks the stream the player would open out of an item
// reply. hls4 is what the site's own player plays; the older ladders stand in
// where a file lacks it.
func kinoWatchManifest(status int, body []byte) (string, Result) {
	if msg := kinoWatchAPIError(status); msg != "" {
		return "", Result{State: StateError, Detail: msg}
	}
	var reply struct {
		Item struct {
			Videos []struct {
				Files []struct {
					URL struct {
						HLS  string `json:"hls"`
						HLS2 string `json:"hls2"`
						HLS4 string `json:"hls4"`
					} `json:"url"`
				} `json:"files"`
			} `json:"videos"`
		} `json:"item"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return "", Result{State: StateError, Detail: "unreadable item response", Err: err}
	}
	for _, v := range reply.Item.Videos {
		for _, f := range v.Files {
			for _, u := range []string{f.URL.HLS4, f.URL.HLS2, f.URL.HLS} {
				if u != "" {
					return u, Result{}
				}
			}
		}
	}
	return "", Result{State: StateError, Detail: "the item carried no stream"}
}

// classifyKinoWatchStream judges what the CDN did with the stream the API
// issued. The API answering proves nothing about playback: the CDN is a
// separate set of hosts, and the one that is blocked while the site and API
// load fine. The URL carries the CDN location kinopub chose, as loc=.
func classifyKinoWatchStream(stream string, status int, playlist string, err error) Result {
	u, _ := url.Parse(stream)
	res := Result{Region: strings.ToUpper(u.Query().Get("loc"))}
	switch {
	case err != nil:
		res.State, res.Detail, res.Err = StateBlocked, "the API issued a stream, but "+u.Host+" did not answer", err
	case status < 200 || status >= 300:
		res.State, res.Detail = StateError, "HTTP "+itoa(status)+" from "+u.Host
	case !strings.HasPrefix(playlist, "#EXTM3U"):
		res.State, res.Detail = StateError, u.Host+" answered with something other than a playlist"
	default:
		res.State, res.Detail = StateAvailable, "served by "+u.Host
		if top := twitchTopRendition(playlist); top != "" {
			res.Detail = top + " from " + u.Host
		}
	}
	return res
}

// kinoWatchPlayer follows the path a signed-in app takes to play something —
// a title off the popular list, its stream, the playlist from the CDN — and
// needs the account's token because none of it is served anonymously.
func kinoWatchPlayer(token string) Check {
	return Check{
		ID: "kinowatch_player", Name: "kino.watch player",
		Run: func(ctx context.Context, env Env) Result {
			api := func(path string) (*netx.Response, error) {
				return env.Stack.Do(ctx, env.Family, netx.Request{
					URL:       kinoWatchAPI + path,
					UserAgent: browserUA,
					Headers:   map[string]string{"Authorization": "Bearer " + token},
				})
			}
			resp, err := api("items/popular?type=movie&perpage=1")
			if err != nil {
				return Result{State: StateError, Detail: "request failed", Err: err}
			}
			if msg := kinoWatchAPIError(resp.Status); msg != "" {
				return Result{State: StateError, Detail: msg}
			}
			id := jsonx.String(resp.Body, "items.0.id")
			if id == "" {
				return Result{State: StateError, Detail: "the popular list was empty"}
			}

			resp, err = api("items/" + id)
			if err != nil {
				return Result{State: StateError, Detail: "request failed", Err: err}
			}
			stream, res := kinoWatchManifest(resp.Status, resp.Body)
			if stream == "" {
				return res
			}

			resp, err = env.Stack.Do(ctx, env.Family, netx.Request{URL: stream, UserAgent: browserUA})
			status := 0
			if resp != nil {
				status = resp.Status
			}
			return classifyKinoWatchStream(stream, status, resp.Text(), err)
		},
	}
}
