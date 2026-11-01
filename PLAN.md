# SSH-Cloud: Plan

A Windows app (Go + [windigo](https://github.com/rodrigocfd/windigo)) that shows an
SSH/SFTP server as an Explorer-like window. Browsing, renaming, moving and
deleting happen directly on the server. There is no sync and no editing in the
cloud: you **drag a file out** to work on it locally and **drag it back** to upload.
A local transfer list is used to detect collisions.

## Constraints

- Windows 10/11, native Win32 controls via windigo, styled like Windows 11.
  Single `.exe`.
- Server: plain OpenSSH SFTP. No shell, no daemon. Server-side copy needs the
  `copy-data` extension (OpenSSH 9.0+); everything else works on any SFTP server.
- Typically one shared SSH account with a key per person in `authorized_keys`;
  password login is supported too. Passwords and passphrases are never stored.
- File content only moves after an explicit user action (drag, menu, prompt).
- Collision detection uses SHA-256 hashes only, never clocks or mtimes.
- Every server change is atomic: a single SFTP rename. Uploads go to a temp
  file first. The final rename never overwrites (plain SFTP `rename` fails if the
  target exists), so two uploads can never both land unnoticed.
- No locks, no ownership. The user decides on every collision.
- Folders are only namespaces: created on upload, removed when empty.

## Hash files

Each file has a tiny hidden hash file next to it, `.<name>.sha256`, holding the
SHA-256 and size of the current content. It lets the client compare versions
without downloading the file. It is only written by the client that just put a
new version in place, and it moves, copies and gets deleted along with its file.
A missing hash file means "unknown version".

## Workflow

**Download** (drag out, or double-click / "Download to..."):
stream the file, compute SHA-256 from the bytes actually read, and record it in
the transfer list. (If the hash file disagrees, it is stale; the next upload will
then prompt, which is the safe side.)

**Upload** (drag back), per file:

1. If the local hash equals the latest transfer list entry and the server's hash
   file still has that hash: unchanged, skip.
2. Upload to a temp file `.<name>.tmp-<random>` next to the target, hashing while
   sending.
3. **Target does not exist**: rename temp to target, then write the hash file. If
   the rename fails because the target now exists, someone else just created it:
   go to the prompt.
4. **Target exists**: claim it by renaming it to `.<name>.claim-<random>` with
   `posix-rename` (one atomic `rename(2)` on the server; the claim name is
   unique, so nothing is overwritten). Only one client can win this rename; a
   loser retries for a moment and then sees the winner's new version. (Plain
   SFTP rename is not used here: OpenSSH implements it as link + unlink, which
   is not exclusive for a contested source.)
   - The hash file's hash equals the transfer list entry (and its size equals the
     claimed file's size): nobody else changed it. Delete the hash file, rename
     temp to target, write the new hash file, delete the claim.
   - Otherwise (different hash, no hash file, or never transferred by this
     client): rename the claim back and ask **Replace / Keep both / Skip**
     (option: apply to all).
5. Record the uploaded hash in the transfer list.

Two rules keep step 3 and step 4 apart (a folder listing is not an atomic
snapshot, so a client can miss another client's claim and see the file as
missing while it is only claimed):

- An upload that started on an **existing** file never takes the new-file path.
  If the file is missing at commit time it was deleted, moved, or is claimed
  right now: prompt ("deleted, moved or being replaced during your upload").
- An upload that started as a **new** file never claims. If the file exists at
  commit time, someone created it meanwhile: prompt.

Why simultaneous uploads always prompt: only one client wins the claim, and the
loser then finds a hash file with the winner's hash, which is not in its transfer
list. A new-file race is caught by the non-overwriting rename in step 3. Both
races are stress-tested with eight clients at once.

Why a crash is safe: the hash file is deleted before the new version goes in and
written after it. A crash in between leaves no hash file ("unknown"), so the next
upload prompts. A claim file whose target is missing is renamed back by any client
that sees it; if the claimer is still busy, its own final rename fails and it
prompts.

**Out-of-sync hash file** (the file was changed outside the app, or a crash left
an old hash file behind). Rule: never write a hash for content you have not just
read or written yourself, and only while holding the claim. Detected in three
places:

- **Listing**: the hash file's size differs from the file's size. Shown as
  "Changed on server" in the Status column. A listing never writes anything.
- **Download**: the hash of the bytes just read differs from the hash file, or
  there is none. The transfer list gets the real hash. Then repair: claim the
  file; if the hash file is still the same stale one and the claimed file's size
  equals what was read, write the real hash; otherwise leave it. Release the
  claim. A hash file that changed meanwhile was written by an uploader, so it is
  fresh and is left alone.
- **Upload** (under the claim): the hash file's size differs from the claimed
  file. Delete the hash file (the content is unknown and not read by us), release
  the claim and prompt: "report.docx was changed outside SSH-Cloud". Replace
  writes a correct hash file; Keep both and Skip leave the original as "unknown"
  until someone downloads it (repair) or replaces it.

An unknown version always prompts on upload, so a stale hash file can cost an
extra prompt but never cause an unseen overwrite (apart from the same-size outside
change listed under weaknesses).

**Replace** uses the same claim steps, without the comparison. **Keep both**
renames temp to `<name> (2).<ext>` (next free number) and writes its hash file.

## Connections and login

- Saved connections in `%APPDATA%\SSH-Cloud\config.json`, managed in a
  Connections dialog (connect, new, edit, delete). Each has a stable ID, so a
  rename keeps its transfer list. The app connects to the last used one at
  startup and opens the manager when that fails.
- Authentication per connection:
  - **Automatic** (default): the Windows OpenSSH agent, then Pageant, then the
    default keys `~/.ssh/id_ed25519`, `id_ecdsa`, `id_rsa`, then a password
    prompt. An encrypted default key is offered by its `.pub` key and only
    decrypted (passphrase prompt) when the server accepts it, like OpenSSH.
  - **Key file**: one private key, passphrase prompted if encrypted.
  - **Pageant**: PuTTY's agent, spoken to directly over its WM_COPYDATA protocol.
  - **Password**: password and keyboard-interactive.
- Host keys are checked against `~/.ssh/known_hosts`: unknown hosts are shown
  with their fingerprint to trust, a changed key is always refused.

## Transfer list

- Local, per connection profile, append-only (`%APPDATA%\SSH-Cloud\`).
- Entry: time, direction, remote path, local path, size, SHA-256, result.
- Shown in a Transfers window (newest first, including moves and errors).
- Renames and moves made in the app update the paths in the list.

## Explorer window

- Windows 11 look: Fluent UI System Icons (MIT, embedded SVGs rendered at the
  screen's DPI) on flat toolbars, the Explorer theme on the tree and list,
  Segoe UI Variable (falls back to the system font on Windows 10), a white
  content area with thin dividers, and a rounded window with a Mica title bar.
- Tabs in a grey band at the top, one per open connection, each with its own
  folder, history and listing. New tab (+, Ctrl+T) opens the connection
  manager; close with Ctrl+W, a middle-click or the tab's right-click menu;
  Ctrl+Tab switches. Cut/copy and paste work within one connection.
- Navigation row: Back, Forward, Up, Refresh and the address bar. Command bar:
  New folder, Upload, Download, Cut, Copy, Paste, Rename, Delete, Transfers,
  Stop, and Details and Connections on the right. Thin dividers between rows,
  a plain status line at the bottom, the app icon on the window.
- Details pane (toggle with Details or Alt+Shift+P, remembered): large icon,
  name and type of the selection, then size, date modified, status, last
  transfer, location and permissions; totals for a multiple selection; the
  current folder when nothing is selected.
- Folder tree (loaded as you expand it, kept in step with the list) and a file
  list with Name, Date modified, Type, Size and Status. Real shell icons and
  type names; click a header to sort (folders first). Hash, claim and temp
  files are hidden.
- Status column: "Up to date" or "Changed on server" for files this client
  transferred before (only their small hash files are read, in the background).
- Keyboard: Ctrl+X/C/V, Ctrl+A, Ctrl+Shift+N, F2, Del, F5, Backspace,
  Alt+Left/Right/Up, Ctrl+L or Alt+D for the address bar. While typing in the
  address bar or renaming, editing keys act on the text.
- Drag in from Windows Explorer: upload into the folder under the cursor or the
  current folder. Drag within the list onto a folder: move on the server.
- Drag out to Windows Explorer: a hand-written COM `IDataObject` offers
  `FileGroupDescriptorW` (built from listings only) and `FileContents`; each file
  is downloaded through the transfer engine only when the drop target asks for
  it, so the transfer list records its hash. The first request downloads all
  files of the drag in the progress window (Cancel aborts the drop); later
  requests are answered from those downloads.
- Context menu: Open, Download to..., Cut, Copy, Paste, Rename, Delete,
  Copy path, New folder, Upload, Refresh.
- Server-side copy (Copy then Paste): `copy-data` on a small SFTP session of its
  own (pkg/sftp has no API for it), into a temp file and renamed into place
  without overwriting. Explorer style names ("report - Copy.docx"). The hash
  file is carried over only when it is valid for the source and did not change
  during the copy. Greyed out when the server has no `copy-data`.
- Uploads, downloads, copies and deletes run on a goroutine with a progress
  window (current file, bytes, speed, time left, Cancel). Collision prompts
  open on top of it.

## Updates

An update button (sync icon, next to Details) checks the latest GitHub
release on request. When it is newer, SSH-Cloud downloads the MSI in the
progress window, starts `msiexec` and closes. Nothing is checked
automatically and there is no setting for it. Development builds ("dev")
do not update.

## Logging

All packages log through `log/slog` to `%APPDATA%\SSH-Cloud\ssh-cloud.log`
(rotated at 5 MB): connects, login method and keys used (by fingerprint),
host key decisions, server capabilities, every move, delete, upload, download
and copy, conflicts and the user's choices, hash repairs, reconnects and
errors. Crashes and panics in background work are logged too. Passwords and
passphrases are never logged.

## Architecture

```
cmd/ssh-cloud/         the Windows program (manifest for common controls v6)
cmd/sshcloud-cli/      command-line tool for testing against a real server
internal/remote/       SSH/SFTP connection, login methods, Pageant, atomic ops,
                       folder create/prune, copy-data
internal/transfer/     download, upload, copy, hashing, collision check, progress
internal/transferlog/  local transfer list
internal/app/          window-independent explorer logic: listing, virtual
                       folders, multi-file operations, status, reconnect
internal/config/       saved connections and settings
internal/dragdata/     FileGroupDescriptorW bytes for drag out
internal/icons/        Fluent SVG icons, rasterised at any size and colour
internal/logging/      log file setup
internal/update/       GitHub release check and MSI download
internal/sshtest/      in-process SSH server for tests (OpenSSH rename rules)
internal/ui/           windigo windows, toolbars, dialogs, drag and drop
tools/genicon/         renders the app icon (Fluent cloud) into winres/
tools/mkico/           packs the PNGs into the installer's .ico
winres/                icon, manifest and version info for go-winres
installer/             MSI definition (wixl)
run.sh, release.sh     build and run with Wine; build the MSI and publish
```

Everything outside `ui` is plain Go and tested on Linux against the in-process
server. `ui` is Windows-only and tested by hand.

## Milestones

1. Remote layer: connect, list, rename, move, delete.
2. Transfers: download, atomic upload, transfer list, collision check.
3. Explorer window with download/upload buttons and the collision prompt.
4. Connection manager (saved connections, auto-connect to the last one),
   drag in (upload) and drag within (move), Transfers window, stop button.
5. Drag out to Windows Explorer (COM `IDataObject` with delayed rendering);
   needs testing on real Windows.
6. Polish: folder tree, shell icons and type names, Status column ("changed
   on server" since your last transfer), sortable columns, cut/copy/paste
   (copy is server side with `copy-data`), keyboard shortcuts, and a progress
   window with cancel for uploads, downloads, copies and deletes, which run
   on a background goroutine.
7. Windows 11 look (Fluent icons, toolbars, Explorer theme, font, frame) and
   login methods: automatic, key file, Pageant, password.
8. Release tooling: app icon, MSI installer (wixl) and `release.sh`, ported
   from otp4win. First release: v0.1.0.
9. Explorer polish after the first Windows test: toolbar backgrounds and
   spacing like Windows 11 Explorer, plain status line, window icon, tabs for
   multiple connections, the details pane, and an update button.

Milestones 1 to 9 are done (1 to 8 were released as v0.1.0). Parts that need the Windows UI (drag out, the toolbars and theme,
tabs, the details pane, Pageant, the updater) are only verified by testing on
Windows.

## Known weaknesses (accepted)

- During an upload the file is missing from the server for a few milliseconds
  (between claim and final rename). A download in that moment fails and is
  retried.
- Servers on filesystems without hard links fall back to a stat-then-rename in
  `sftp-server`, which has a tiny race of its own.
- Tools outside the app (`scp`, shell) do not update hash files. A change of the
  same size made that way goes unnoticed; such tools should delete the hash file
  when they change a file.
- The transfer list is per client: uploading from another machine always asks.
- A file that someone creates as new, at the exact moment another client holds
  a claim on an existing file with that name (or a download is repairing its
  hash file), can take the name. The claimed version is then kept as
  "name (2).ext" and that client is prompted; nothing is lost, but the version
  moves without its author being told.
- Pageant checks that the request's file mapping belongs to the current user;
  for an elevated (administrator) process this may fail, as the default owner
  is then the Administrators group.
