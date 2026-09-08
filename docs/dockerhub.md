# geocheck

**Where the internet thinks you are – and how directly you actually reach it.**

Every IP geolocation tool answers _where does the internet think I am_. That is
half the question. The other half is **how your packets actually get there** –
whether you enter Google's network on-net or three transit carriers later,
whether a tunnel is quietly adding 90 ms, whether something is answering on a
destination's behalf. geocheck answers both in one pass, and prints the evidence.

![geocheck](https://raw.githubusercontent.com/remnawave/geocheck/main/docs/img/demo.gif)

## Run it

```sh
docker run --rm -it --network host remnawave/geocheck:ing
```

`--network host` matters. Without it the container gets its own network
namespace, so the trace starts at Docker's bridge instead of your real gateway,
and the exit address is whatever the host happens to SNAT to. On Linux it makes
the measurement describe the host; on macOS and Windows the "host" is a Linux
VM, so it shares that VM's namespace instead and buys you less.

```sh
# IPv4 only, with every hop of every path
docker run --rm -it --network host remnawave/geocheck:ing -4 -d

# Machine-readable
docker run --rm --network host remnawave/geocheck:ing --json > report.json

# Just "is my connection direct" – skip the geolocation half
docker run --rm -it --network host remnawave/geocheck:ing --no-geo

# A sample report, measuring nothing
docker run --rm -it remnawave/geocheck:ing --demo
```

Use `-it` so colours and the progress line render; drop it when piping.

Under **podman** add `--cap-add=NET_RAW`. Docker grants that capability by
default and podman does not, and without it the path trace degrades to
destination latency only, with no hops:

```sh
podman run --rm -it --network host --cap-add=NET_RAW remnawave/geocheck:ing
```

There is also a launcher that picks a runtime for you, installs nothing and
cleans up after itself:

```sh
curl -fsSL https://geocheck.ing | sh
```

## Tags

| Tag      | What it is                                                |
| -------- | --------------------------------------------------------- |
| `ing`    | The current release. This is the one to use.              |
| `latest` | The same image, for tooling that assumes `latest` exists. |

Pre-releases are published under their exact version and never move `ing`.

## What it measures

- **Address reputation** – datacenter or residential, known VPN or proxy, risk
  score. Usually the thing that explains a refusal the geolocation table cannot.
- **Geolocation consensus** – ~40 GeoIP APIs and consumer services, and where
  they disagree.
- **Connectivity checks** – the endpoints operating systems use to decide they
  are online (Google `generate_204`, Apple hotspot-detect, Microsoft NCSI),
  each compared against the response its vendor specifies.
- **Path analysis** – the route to the major networks, every hop annotated with
  its autonomous system, and a verdict on how directly each is reached:
  `direct / on-net`, `regional`, `transit`, `detour` or `intercepted`.
- **Service availability** – whether Netflix, ChatGPT, Gemini, NotebookLM, Twitch,
  YouTube Premium, Claude and TikTok will actually serve you.

It also reports when the measurement itself is being tampered with – a tunnel
carrying your default route, a resolver answering on someone else's behalf –
because those invalidate everything else.

## Image

Multi-architecture: `linux/amd64` and `linux/arm64`. The binary is static, the
image carries `cap_net_raw` in its permitted set, and it still runs under
`--cap-drop=ALL` (reporting latency only, and saying so).

## Links

- Source, full documentation and options:
  [github.com/remnawave/geocheck](https://github.com/remnawave/geocheck)
- Releases and binaries:
  [github.com/remnawave/geocheck/releases](https://github.com/remnawave/geocheck/releases)
- Licence: MIT
