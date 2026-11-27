# techo5-plex

A Plex player add-on for [TECHO5](https://github.com/HuskerMinion/techo5). It makes an Echo Show running
TECHO5 show up as a player in Plex apps, so you can cast music and video from Plex to it. What you cast is
played by the device's own DLNA renderer, so songs come out of the speaker with Now Playing and films play
full screen, the same way as any other DLNA cast. On a TECHO5 with the add-on API, it also takes voice
commands from Home Assistant: "play The Office on Plex" plays the next episode on the device itself (see
**Voice**).

This is an add-on, not part of TECHO5: it runs from the device's storage at boot, outlasts updates, and is
only on devices whose owners install it.

## Credit

The Plex side of this add-on comes from **kevin909867**'s `plexcast`, proposed for TECHO5 in
[PR #691](https://github.com/HuskerMinion/techo5/pull/691): the plex.tv PIN link, GDM discovery, the
Companion control server, the transcode request and the timelines. It has been moved out of the TECHO5
daemon into this standalone program, and changed to drive the device's DLNA renderer instead of
decoding media itself. Thank you, Kevin.

Playing by voice is **stoney66**'s method (Stoney, on the TECHO5 Discord): he voiced Plex on his
devices through Home Assistant scripts generated for each show in his library, and this add-on now does
that itself. Casting from Plex apps is HuskerMinion's and kevin909867's; playing by voice is Stoney's. The way titles are matched comes from what he learned doing it: a leading "the" comes and
goes in speech, and a title with its year in it, like "Doctor Who (2005)", never matched what anybody
says. Thank you, Stoney.

## What it does

- **Discovery.** Answers Plex's GDM searches (UDP 32412, multicast 239.0.0.250 or sent to the device
  directly) on Wi-Fi and announces itself (HELLO and BYE to 32413), which is how Plex apps on the same
  network find a player. Multicast does not cross from one subnet to another, so the same HELLO and BYE
  also go by unicast, every 30 seconds, to each Plex Media Server it knows of: those the linked account
  lists with a local address (read again every ten minutes) and any server something was cast from.
  Plex Web reaches players through the server, so this is what lists the player there when the server
  and the device are on different subnets. The log names the servers announced to.
- **Control.** Runs the Plex Companion server on TCP 32500: `playMedia`, play, pause, stop, seek, skip,
  volume, audio and subtitle tracks, and timelines (polled or pushed).
- **Next and Previous on the device.** With a TECHO5 whose DLNA renderer reports them (a version with
  this support; older ones do not), pressing Next or Previous on the device's own Now Playing skips
  through the Plex queue, as the app's buttons do. The add-on subscribes to the renderer's events on the
  device itself for this, and hears a pause or stop on the device sooner from them too.
- **Seeking on the device.** With a TECHO5 whose screen has a seek bar, a place tapped or dragged to on
  the bar under a film or on Now Playing moves playback there, as a seek in the app does (a film's
  stream starts again from that place, which takes a few seconds).
- **Account link (needed for films).** Links a Plex account with a code you enter at
  [plex.tv/link](https://plex.tv/link), and tells the account where the player is. Some Plex apps only
  list the players your account knows about.
- **Playback.** Asks your Plex Media Server for something the device can play and hands it to TECHO5's
  DLNA renderer on `127.0.0.1:8181`:
  - music: MP3 and FLAC as they are; anything else (AAC, ALAC, Opus...) as MP3 from the server;
  - video: H.264 from the server as one continuous stream that starts at the point you play from (a
    resume starts where you left off), no larger than 1280x720, with subtitles burned in when you turn
    them on. Keep that size on a Skylight too: its screen is larger, but at 1920x1080 it decodes only
    about 20 frames a second and drops some.
- **Progress.** Tells the server where playback is every ten seconds, so the resume point and
  "Continue Watching" stay current, and answers controllers' timeline polls so their clock and progress
  bar follow along.
- **A small status page** on TCP 32501: whether the account is linked, the code to enter while linking,
  whether the DLNA renderer was found, and what is playing, with **Link**, **Cancel** and **Unlink**
  buttons.

It does not decode anything itself and does not need the internet to cast on your own network.

## What you need

- A device running TECHO5's Linux build with a screen: the Echo Show builds, including the Echo Show 8 2nd
  gen and the Skylight. (The Echo Spot may work; it has not been tried.) It needs **TECHO5 v1.6.52 or
  later**, whose DLNA renderer plays what a program on the device casts without asking on the screen, and
  can seek in a film.
- **DLNA turned on** in TECHO5 (setup page, **Sound → Play to this device → DLNA**). For films, also
  **Screen → Video → Play videos** and **DLNA video**.
- A **Plex Media Server** on the same network. Plex apps (Plex Web included) cast from a server on your
  network; this add-on fetches from that server and never from plex.tv's own content.
- SSH access to the device, for the install by hand (not for the setup page's).

## Install from the setup page

On a TECHO5 version with the **Add-ons** card (setup page, **System → Add-ons**), install it from there:
the card lists it once TECHO5's add-on catalog has a release of it that carries its manifest
(`addon.json`, shipped with every release after v0.1.0). Tick the box under the warning, and press
**Install**. TECHO5 then:

- downloads the binary for your device and checks it against the catalog;
- runs it as a user of its own, not as root, with its state in
  `/data/techo5-addons/techo5-plex/state`;
- opens its ports (32500 and 32501 TCP, 32410 to 32414 UDP) on Wi-Fi only while it runs, and closes them
  when it stops;
- restarts it if it stops, and offers each new release as an update, on the card and in Home Assistant.

The card also has its log, **Stop**, **Remove**, and a link to its page for linking your Plex account.

**Switching from the rc.local install:** remove the lines for techo5-plex from
`/data/techo5-linux/rc.local` (the `PLEX-IN` firewall lines and the line that starts it), reboot, and then
install it from the card. Its state is not carried over: link your account again on its page. The old
folder, `/data/techo5-linux/plex`, can be deleted once it works.

## Install by hand (rc.local)

This is the manual way, and still works as it always has.

1. Download the binary for your device from the [latest release](https://github.com/HuskerMinion/techo5-plex/releases/latest).
   Each one is built and attested by this repository's CI; check one with
   `gh attestation verify techo5-plex-arm --repo HuskerMinion/techo5-plex`. Pick the one for your device. `techo5-plex-arm` (32-bit ARM) runs on every Echo Show build,
   including the Echo Show 5. `techo5-plex-arm64` is for the boards with a 64-bit kernel (Echo Show 8 2nd
   gen, Skylight); if you are not sure, use `techo5-plex-arm`.

2. Copy it to the device:

   ```sh
   ssh root@<address> mkdir -p /data/techo5-linux/plex
   scp techo5-plex-arm root@<address>:/data/techo5-linux/plex/techo5-plex
   ssh root@<address> chmod 755 /data/techo5-linux/plex/techo5-plex
   ```

3. Start it at boot. Add these lines to `/data/techo5-linux/rc.local` (see
   [Running something of your own at boot](https://github.com/HuskerMinion/techo5/blob/main/docs/setup.md#running-something-of-your-own-at-boot);
   make the file and `chmod 755` it if you do not have one yet). The first part opens the ports the
   add-on uses in the device's firewall, which otherwise drops them on Wi-Fi; the last line starts it:

   ```sh
   #!/bin/sh
   # techo5-plex: Plex discovery (UDP 32410-32414), control (TCP 32500) and its page (TCP 32501)
   for T in iptables-legacy ip6tables-legacy; do
   	$T -N PLEX-IN 2> /dev/null
   	$T -F PLEX-IN
   	$T -A PLEX-IN -p tcp --dport 32500 -j ACCEPT
   	$T -A PLEX-IN -p tcp --dport 32501 -j ACCEPT
   	$T -A PLEX-IN -p udp --dport 32410:32414 -j ACCEPT
   	$T -C INPUT -i wlan0 -j PLEX-IN 2> /dev/null || $T -I INPUT 1 -i wlan0 -j PLEX-IN
   done
   /data/techo5-linux/plex/techo5-plex >> /data/techo5-linux/plex/plex.log 2>&1 &
   ```

   The rule must be inserted at the top of `INPUT` (`-I INPUT 1`), ahead of TECHO5's own chain, which
   ends in a drop. Use the `-legacy` tools: the kernel has no nftables. It has no `multiport` either, so
   each port has a rule of its own.

4. Reboot, or run the same lines once by hand.

The add-on keeps its state (the player's identifier and the linked account's token) in
`/data/techo5-linux/plex/state.json`, readable only by root. Its log does not contain the token.

## Link your Plex account

Linking is needed for films, and for changing a film's audio or subtitles while it plays. The device
plays a film the add-on hands it without asking on the screen, and the add-on takes commands from any
device on your network, so it plays films only from a Plex server your account lists, at an address the
account lists for it, and records track changes on that server with your account's own access to it.
Music plays without a link, from any server. Some apps, mostly on phones, also list only the players
your account knows about.

1. Open `http://<address>:32501/` from a phone or computer on the same network.
2. Press **Link**. A four-character code appears (it is also written to the log).
3. Go to [plex.tv/link](https://plex.tv/link), sign in, and enter the code. The page says **Linked**
   within a few seconds.

**Unlink** on the same page forgets the account and takes the player out of its device list.

## Cast to it

Open a Plex app on the same network, start something, and pick the device's name from the cast or player
list. The name is the one TECHO5 gives its DLNA renderer; start the add-on with `-name "Kitchen"` to use
another.

Which app to use:

- **Music: Plexamp.** It finds the player through the linked account, and plays, pauses and skips.
- **Films and TV: Plex Web**, from the player button. If the device is not listed, link the account (so
  the add-on learns your servers) and give it a minute. Open Plex Web from your own server
  (`https://<your server>:32400/web`, or the server's plex.direct address) rather than app.plex.tv if
  you want its progress bar to move: see **Known limits**.
- **Not the Plex phone app's cast button.** That lists only Google Cast and Vizbee devices, so the
  player is never there.

Seeking is done by the renderer where it can. Otherwise, as with every film, the add-on asks the server
for a new stream that starts at the new position, which takes a few seconds. Changing the audio or
subtitle track during a film works the same way: the film restarts at the same second with the new
track.

## Voice

On a TECHO5 that has the add-on API (`api1`), installed from the setup page, the add-on also takes
commands from Home Assistant, and plays what they ask for on the device itself, full screen,
with TECHO5's own video player. No Plex app is involved, and DLNA does not have to be on; **Screen →
Video → Play videos** does. On install, the consent screen says it "Adds commands Home Assistant and
voice can call: *play, play_episode, pause, resume, stop, next_episode, restart, choose*", "Plays video
and audio from the network on this device" and "Adds entities to Home Assistant: *binary_sensor.asking,
binary_sensor.paused*". Voice needs a linked account (**Link your Plex account**). Installed by hand (rc.local), the add-on has
no API to take commands from, and is the player it always was.

| Command | Arguments | Does |
|---|---|---|
| `play` | `title`, `from_start`, `if_known` | A show goes on with the episode you are up to: one started and not finished, else the first not watched after the last one watched, else the first episode. A film plays. One left part way is not started: the add-on asks whether to resume it or start from the beginning (see below). With `from_start` true, or a title that ends in "from the beginning" or "from the start", it starts at the beginning without asking. With `if_known` true it plays only a title the library has exactly, and answers nothing (`""`) for anything else, at once, without reading the library while Home Assistant waits. |
| `play_episode` | `title`, `season`, `episode`, `from_start` | That episode of the show, asking as `play` does when it was left part way. The numbers may be digits or words ("4", "four"). |
| `choose` | `answer` | The answer to that question, as said: "resume", "continue", "keep going", "where I left off" or "yes" resume it; "from the beginning", "the start", "start over", "restart" or "no" start it from 0:00. "And", "um", "uh", "please" and "the" around it do not matter. With no question waiting: "There's nothing to choose." |
| `pause`, `resume`, `stop` | | What voice started. Stop keeps its place on the server. `resume` with a question waiting answers it: it resumes the item asked about. `stop` with a question waiting drops it. |
| `next_episode` | | The episode after the one playing. |
| `restart` | | What voice played last, playing or stopped, from its beginning. |

Each command answers with a short sentence for Home Assistant to say: "Playing The Office, season 2
episode 4.", "Resuming Project Hail Mary at 1 hour 10 minutes.", "I couldn't find Zardoz in your Plex library.", "Nothing from Plex
is playing." It answers so even when it could not do what was asked, so that a voice command always says
something; the action fails only when the add-on is not running. A film that takes the server more than
about three seconds to start is answered "Starting ... from Plex." and starts a moment later.

**Resume, or start over?** Asked for something left part way, the add-on answers with a question and
plays nothing yet: "Project Hail Mary is 1 hour 10 minutes in. Resume, or start from the beginning?"
The package says the first part as the reply; then the device asks the question itself
(`assist_satellite.ask_question`, in the package's `techo5_plex_answer` script) and listens for the
answer without its wake word: "resume", "yes", "keep going", or "from the beginning", "start over",
"no", or "stop" / "never mind" to start nothing. A filler word on its own ("okay", which speech-to-text
also makes of near silence) is no answer. The question waits 30 seconds, so "resume" said
later, with the wake word, still answers it. While it waits, the add-on's `asking` sensor is on, and the package sends an
answer to `choose` only then. Its
`paused` sensor is on while what voice plays is paused, so a bare "resume" goes to Plex only when Plex
is what is paused; otherwise it resumes whatever the device has paused, music or the radio. "Stop plex"
while the question waits drops it.

**How titles are matched.** Against every film and show in the video sections of your servers on the
home network (the account's own first, then shared ones), read when the account is linked, every hour,
and again whenever a title is not found in a list more than two minutes old, so something just added is
found. Upper and lower case, punctuation, accents and a leading "the" do not matter, numbers may be
words ("rocky two", "oceans eleven"), a year in brackets at the end of a title is ignored ("Doctor Who
(2005)" is "doctor who"), and near misses of speech to text still match ("the ofice"). When two have the
same title, say the year to pick one ("dune 1984"); without it the newest plays, and the answer says the
year: "Playing Dune (2021)." A film and a show of the same title and year are told apart in the answer
as "the movie" or "the show".

**The stream** is the one a cast gets: the server is asked for H.264 no larger than `-video-size`
(1280x720), copying what the device decodes and converting the rest, from the place it starts at. The
server is told what the device says of it: playing, paused, and where it is every ten seconds while it
plays. When it stops, the server is told where: at its end when it played to it, which marks it watched;
where it was when it was stopped on the device, replaced or failed, and the server marks it watched only
if that was near its end, as it does for any player. So "Continue Watching" and the next episode stay
right. The add-on asks the device again after it reconnects, so an end it missed meanwhile is reported
too.

### In Home Assistant

The add-on cannot hear: speech is Home Assistant's to recognize, and it sends the add-on the command.
So one file on the Home Assistant side says which sentences mean which command:
[`homeassistant/techo5_plex.yaml`](homeassistant/techo5_plex.yaml). No script per show, and nothing else
to write.

1. Copy it to `config/packages/techo5_plex.yaml`. If `configuration.yaml` has no packages yet, add:

   ```yaml
   homeassistant:
     packages: !include_dir_named packages
   ```

2. Restart Home Assistant.

It works on every device with the add-on, and plays on the device you spoke to, found the way Stoney
does it: the device that heard the sentence, else the one whose voice satellite is answering. Typed in
Assist instead, it uses the device named in `fallback:` in the file, if you set one.

It understands "play {title} on plex" ("from plex" and "in plex" too; also with "from the beginning",
or "season {n} episode {n}", before "on plex"), "pause plex", "stop plex", "next episode", "resume"
and "start over", and the answers to the add-on's question. A filler word first ("and play ...", "um",
"so", "okay") is fine. An answer goes to `choose` only while the `asking` sensor is on. "Resume" goes
to Plex while Plex is paused, and otherwise resumes whatever the device has paused, or says "Nothing
from Plex is waiting." "Start over" with no question waiting restarts what Plex played last.

Home Assistant runs every sentence-trigger automation that matches what was said, at the same time,
and says the first answer. If you have an automation of your own that catches every "play {query}",
it runs for "play ... on plex" too: give it the condition shown at the top of the file, so it leaves
Plex's sentences to this one.

**Titles without "on plex".** Stoney's way is "play bluey", nothing more. With an automation of your
own that catches every "play {query}", add the step shown at the top of the file as its first action:
it calls `play` with `if_known`, which plays the title only if it is a film or show in your library,
matched exactly once normalized ("fifty first dates" is 50 First Dates), and otherwise answers nothing
at once, so your automation goes on to music. A film with the same name as a song goes to Plex; "...
on spotify" or naming a speaker keeps it for music if your automation handles those first.

Home Assistant names the add-on's entities after its name in the catalog, Plex Player:
`binary_sensor.kitchen_plex_player_asking`, `binary_sensor.kitchen_plex_player_paused`, and a button
for each command without arguments (`button.kitchen_plex_player_pause` and so on), for a dashboard.
The commands can also be called from any script or automation with the `addon_command` action (see
[actions.md](https://github.com/HuskerMinion/techo5/blob/main/docs/actions.md)): `title`, `season`,
`episode` and `answer` are strings, `from_start` is `true` or `false`.

### When titles are misheard

Speech-to-text hears what it expects: "play chuck" can come back as "play check", which is no title,
so it goes to music. If you use Plex by voice a lot and run wyoming-faster-whisper, its
`--initial-prompt` can list the titles your household says most (Whisper takes about 220 tokens,
some 40 to 60 titles, not a whole library). It applies to everything said to every device, and
words in the prompt can turn up when Whisper hears near silence, so keep the list short and try a
few ordinary commands before and after.

### What voice keeps and writes

The library, the server's transient tokens and what is playing are kept in memory only. A command writes
nothing to the flash: `state.json` is written only when the account is linked or unlinked, and a command
logs only at debug level (an add-on's log is a file on the device's storage), so with `-debug` off the
add-on's writes for voice come to a line in its log each time it connects to TECHO5, about none a day.

## Known limits

- **Plex Web on app.plex.tv: the progress bar and clock do not move.** Plex Web reaches the player
  through your server, and only accepts the player's timeline when it can read a header naming the
  player. The server's relay does not let the browser read it, so Plex Web discards every update.
  Playing, pausing, seeking and stopping all still work. Plex Web opened from your own server
  (`/web` on port 32400) reads the header, and there the bar and clock follow along.
- **Plex Web's playback settings menu crashes while casting.** Opening it during a film shows "Playback
  Error" and stops the film. This is a long-standing Plex Web bug that happens with every player it
  casts to, not only this one. To choose audio or subtitles, do it on the film's page before you cast,
  or change them during the film from a Plex phone app used as a remote.
- **The device's name and its ESPHome name have to match.** The package finds a device's add-on by its
  `binary_sensor.<name>_plex_player_asking` entity and calls `esphome.<name>_addon_command`: the first
  follows the device's name in Home Assistant, the second its ESPHome name. They match unless the
  device or that entity was renamed; if they don't, set `fallback:` in the package to the ESPHome name.
- **One command at a time.** A play that takes long (a large library read again because a title was
  not found) holds the next command, a pause say, until it is done, for at most 45 seconds.
- **Changing tracks needs a linked account.** The server keeps a film's chosen tracks per file, and
  only takes a change from the account's own token, not from the one a casting app passes. Without a
  linked account, tracks chosen before casting still work.

## Options

| Flag | Default | |
|---|---|---|
| `-data` | `/data/techo5-linux/plex` | where `state.json` is kept |
| `-iface` | `wlan0` | the interface GDM listens and announces on |
| `-renderer` | `http://127.0.0.1:8181/dlna/device.xml` | the DLNA renderer's description |
| `-name` | the renderer's name | what Plex lists the player as |
| `-port` | `32500` | the Companion port |
| `-page-port` | `32501` | the status page's port, `0` for none |
| `-video-size` | `1280x720` | the largest picture asked of the server |
| `-device` | `Echo Show` | the model the Plex account records |
| `-debug` | off | log at debug level |

## Security notes

- Plex Companion control has no password, by Plex's design: anyone on your network can tell the player
  to play something. The add-on only takes commands from private network addresses, only at the
  device's address (not a name, which blocks DNS rebinding from a web page), and never fetches from the
  device itself, loopback or link-local addresses, whatever a command names.
- The Plex token a casting app sends is passed to your server as a header. The only place it appears in
  a URL is the stream address handed to the DLNA renderer, which fetches it with a plain GET; that
  address is never logged. Note that TECHO5's renderer reports the address it is playing to DLNA
  controllers on the network, as every renderer does.
- No token is ever in a timeline.
- To record a track change, the add-on asks plex.tv for the linked account's own token for that server,
  and sends it only to an address the account lists for that server, so a controller on the network
  cannot have it sent anywhere else.
- Voice fetches the library with the account's access token for each server, as a header. For the
  stream address handed to the device's player, it asks the server for a transient token
  (`/security/token`, which the server forgets after two days or a restart), so the account's own token
  never leaves the add-on. That address is the only thing with a token in it sent to TECHO5, and TECHO5
  never logs media addresses. Voice plays only from the account's servers, at their home-network
  addresses.
- The status page's buttons only work from the page itself.
- Films play only from a Plex server the linked account lists, at an address the account lists for it.
  TECHO5 plays a film that a program on the device itself hands its renderer without asking on the
  screen, and this add-on relays what any device on the network sends, so without this check anyone on
  the network could put a film on the screen without the question.

## Build

Go 1.27 or later (the add-on API's client is TECHO5's own `echod/pkg/addonapi`):

```sh
./build.sh            # bin/techo5-plex-arm and bin/techo5-plex-arm64, static, with addon.json and SHA256SUMS
go test ./...
```

## License

MIT, as TECHO5 is, which the original code was contributed under. See [LICENSE](LICENSE).
