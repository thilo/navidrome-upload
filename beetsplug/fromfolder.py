"""Fill empty tags from where an upload put the file, before matching.

The two folders above a file are taken as Artist/Album (a CD1 or Disc 2 folder
below them sets the disc instead); the file name gives the track number and
title, and "Artist - Title" when no folder named the artist. Only empty fields
are set, so tagged files keep their tags.
"""

import os
import re

from beets import plugins

TRACK = re.compile(r"^\(?(\d{1,3})\)?(?:[\s._-]+|$)")
DISC = re.compile(r"^(?:cd|dis[ck])[\s._-]*(\d{1,2})$", re.IGNORECASE)
# Names players use for missing tags, in folders and file names; they name nothing.
PLACEHOLDERS = {"unknown album", "unknown artist", "unbekanntes album", "unbekannter interpret", "(tag missing)"}


def guess(rel):
    """Tags guessed from a path relative to the import root."""
    *dirs, name = rel.split(os.sep)
    stem = os.path.splitext(name)[0]
    tags = {}
    if dirs and (m := DISC.match(dirs[-1])):
        tags["disc"] = int(m[1])
        dirs.pop()
    dirs = ["" if d.lower() in PLACEHOLDERS else d for d in dirs]
    if dirs and dirs[-1]:
        tags["album"] = dirs[-1]
    if len(dirs) > 1 and dirs[-2]:
        tags["artist"] = dirs[-2]
    if m := TRACK.match(stem):
        tags["track"] = int(m[1])
        stem = stem[m.end():] or stem
    if "artist" not in tags and " - " in stem:
        artist, stem = stem.split(" - ", 1)
        if artist.strip().lower() not in PLACEHOLDERS:
            tags["artist"] = artist.strip()
    tags["title"] = stem.strip()
    return tags


class FromFolderPlugin(plugins.BeetsPlugin):
    def __init__(self):
        super().__init__()
        self.register_listener("import_task_start", self.fill)

    def fill(self, task, session):
        roots = [os.fsdecode(p) for p in session.paths]
        for item in task.items:
            path = os.fsdecode(item.path)
            root = next((r for r in roots if path.startswith(r + os.sep)), None)
            if root is None:
                continue
            for field, value in guess(os.path.relpath(path, root)).items():
                if value and not item.get(field):
                    item[field] = value


if __name__ == "__main__":
    assert guess("Music/Art1/Alb1/track-1.mp3") == {"album": "Alb1", "artist": "Art1", "title": "track-1"}
    assert guess("Art/Alb/CD2/03 Song.mp3") == {"disc": 2, "album": "Alb", "artist": "Art", "track": 3, "title": "Song"}
    assert guess("03 - Foo Band - Bar Song.mp3") == {"track": 3, "artist": "Foo Band", "title": "Bar Song"}
    assert guess("Mack The Knife.mp3") == {"title": "Mack The Knife"}
    assert guess("Mix/Loose - Song.mp3") == {"album": "Mix", "artist": "Loose", "title": "Song"}
    assert guess("01.mp3") == {"track": 1, "title": "01"}
    assert guess("Madonna/Unknown Album/Secret.mp3") == {"artist": "Madonna", "title": "Secret"}
    assert guess("Unknown Artist/Unknown Album/U2 - One.mp3") == {"artist": "U2", "title": "One"}
    assert guess("(Tag Missing) - Das Huhn.mp3") == {"title": "Das Huhn"}
    assert guess("(02)  And One - Für.mp3") == {"track": 2, "artist": "And One", "title": "Für"}
    print("ok")
