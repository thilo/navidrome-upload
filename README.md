# navidrome-upload

A browser upload page for [Navidrome](https://www.navidrome.org/). Drop audio
files, folders or zip archives on `https://<your navidrome>/upload/`;
[beets](https://beets.io/) tags them and files them into a library as
Artist/Album, tracks that are already there are skipped, and Navidrome scans
the new folders. It uses Navidrome's own admin logins, so there are no extra
accounts.

Pre-release, tested with Navidrome 0.64.2.

## Features

- Upload from any browser, phone included: single tracks, whole folders or zip
  archives (mp3, flac, m4a, ogg, opus).
- Resumable uploads: an upload continues after a dropped connection, and after a
  page reload when the same file is added again.
- Tagging by beets: tracks are grouped into albums by their tags, retagged from
  MusicBrainz where the match is certain, and filed as `Artist/Album/NN
  Title`. Matched albums get a `cover.jpg` from the Cover Art Archive.
- No duplicates: a track already in any Navidrome library is not added again,
  however it got there.
- A targeted scan: Navidrome scans only the folders an upload wrote, also when
  the page was closed meanwhile.
- No accounts of its own: anyone logged in to Navidrome's web UI as an admin can
  upload.
- Idle at a few MiB of memory; beets runs only during an import, one at a time.

## How it works

One container with a Go server, the page, and beets.

1. The page is served under `/upload/` on Navidrome's own domain, so it can read
   the login token Navidrome's web UI keeps in the browser. Each request carries
   it, and the server checks it with Navidrome (see
   [Security model](#security-model)).
2. Files go up over [tus](https://tus.io/) in 16 MiB chunks into a staging
   volume. The page uses [Uppy](https://uppy.io/); the server uses
   [tusd](https://github.com/tus/tusd) as a library.
3. When a batch is complete, the page asks for an import. The server unpacks
   zips, checks every file, and runs `beet import` into a scratch folder on the
   staging volume.
4. Each tagged track is checked against Navidrome's index (see
   [Tagging and duplicates](#tagging-and-duplicates)); new ones move into the
   library folder.
5. The server calls Navidrome's `startScan` for the folders it wrote. The page
   shows the result: folders written, files skipped and why, and beets' output.

## Compose

```yaml
services:
  navidrome:
    image: deluan/navidrome:0.64.2
    volumes:
      - ./music:/music:ro
      - navidrome_data:/data

  navidrome-upload:
    build: https://github.com/thilo/navidrome-upload.git#v0.1.0
    # Go reaps beets, but not the healthcheck's leftover processes.
    init: true
    # The owner of the music files: the upload writes them.
    user: "1000:1000"
    environment:
      NAVIDROME_URL: http://navidrome:4533 # the default
      MUSIC_DIR: /music                    # the default
    volumes:
      # The same path as in the navidrome service: library paths come from
      # Navidrome. Writable here, read-only for Navidrome.
      - ./music:/music
      - upload_staging:/staging
    read_only: true
    tmpfs:
      - /tmp
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges:true
    healthcheck:
      test: ["CMD", "python3", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=5)"]
      interval: 30s

volumes:
  navidrome_data:
  upload_staging:
```

The container needs outbound HTTPS for MusicBrainz and the Cover Art Archive,
and reaches Navidrome at `NAVIDROME_URL`.

| Variable        | Default                  | Meaning                                                        |
| --------------- | ------------------------ | -------------------------------------------------------------- |
| `NAVIDROME_URL` | `http://navidrome:4533`  | Navidrome as seen from this container                          |
| `MUSIC_DIR`     | `/music`                 | Where both containers mount the music; libraries must be below |

### Reverse proxy

The upload page must share Navidrome's origin. Route `/upload/` on Navidrome's
domain to this container on port 8080 and everything else to Navidrome, and
keep the `/upload` prefix. With Caddy:

```
music.example.com {
	redir /upload /upload/
	handle /upload/* {
		reverse_proxy navidrome-upload:8080
	}
	handle {
		reverse_proxy navidrome:4533
	}
}
```

Other proxies need the same two routes, a request-body limit above 16 MiB on
`/upload/`, and `X-Forwarded-Proto`/`X-Forwarded-Host` passed on, which tus
uses to build upload URLs. Only Caddy has been tested. Navidrome has to be
served at the root of its domain: the page does not support `ND_BASEURL`.

## Tagging and duplicates

beets runs without prompts. All files of one import are grouped into albums by
their album and artist tags, wherever they came from: a zip, a folder or single
files. An album whose tracks arrive in several parts of one upload is merged.
MusicBrainz tags are applied only to confident matches; everything else keeps
its own tags. Cover art comes from the files themselves or, for a MusicBrainz
match, from the Cover Art Archive; image uploads are refused. The settings are
in [beets.yaml](beets.yaml).

Before a track moves into the library, the server asks Navidrome's song API
whether it already has it, in any library:

- the same MusicBrainz recording, or
- the same title and album by the same artist or album artist.

Such tracks are skipped and listed, as are repeats within one upload. Navidrome
indexes everything in its music folder, so files copied there by other means
count too, once Navidrome has scanned them. A different version of a song (a
remaster, a live album) is not a duplicate. A track without title, album and
MusicBrainz tags is never matched; an existing file is still never
overwritten.

## Security model

- **Login.** The page sends the token from Navidrome's web UI. The server asks
  Navidrome's admin-only `/api/library` with it before accepting a request,
  including every upload chunk (cached for 30 seconds). Only Navidrome admins can
  upload. A password change, a removed account or a lost admin role takes
  effect within those 30 seconds; logging out of the web UI does not revoke a
  token Navidrome has issued. Starting a scan needs a Subsonic login, so the page
  passes the Subsonic token the web UI keeps; the server holds it in memory for
  that one import.
- **Same origin.** Sharing Navidrome's origin is what lets the page read the
  login, and it means a script running on the page could too. The page sends a
  strict Content-Security-Policy, loads nothing from other hosts, and the server
  never serves uploaded files back.
- **Uploads.** Before any bytes are accepted: an accepted extension, a declared
  size up to 1 GiB, and room in the 2 GB staging quota, which counts uploads at
  their declared size. Before import, each file's first bytes must match its
  type. Unfinished uploads expire after 24 hours.
- **Zip archives.** Entries that would leave the target folder, links, nested
  zips, images and other non-audio files are skipped and listed; `__MACOSX/`
  and `._*` are dropped. At most 5000 entries, and every unpacked byte counts
  against the quota and the 1 GiB file limit, whatever the archive declares,
  which stops zip bombs.
- **File names** from the browser or a zip never become library paths; beets
  names the files from their tags.
- **Container.** Non-root, read-only filesystem, no capabilities. It writes only
  to the staging volume and, through beets' output, to library folders.

## Test

```bash
go test ./...
```

The Docker build runs it too.

## License

MIT (see [LICENSE](LICENSE)). The image bundles Uppy, tusd and beets, all
MIT-licensed.
