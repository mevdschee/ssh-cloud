# SSH-Cloud: Plan

A Windows app (Go + [windigo](https://github.com/rodrigocfd/windigo)) that shows an
SSH/SFTP server as an Explorer-like window. Browsing, renaming, moving and
deleting happen directly on the server. There is no sync and no editing in the
cloud: you **drag a file out** to work on it locally and **drag it back** to upload.
A local transfer list is used to detect collisions.

## Constraints

- Windows 11 only (no effort for older versions), and it must keep running
  under Wine. Native Win32 controls via windigo plus GDI drawing, styled like
  Windows 11. No new COM beyond what is there (the file dialogs and the drag
  out data object). Single `.exe`.
- Server: plain OpenSSH SFTP. No shell, no daemon. Server-side copy needs the
  `copy-data` extension (OpenSSH 9.0+); everything else works on any SFTP server.
- Typically one shared SSH account with a key per person in `authorized_keys`;
  password login is supported too. Passwords and passphrases are never stored.
- File content only moves after an explicit user action (drag, menu, prompt).
- Collision detection compares the server's own size and modification time of
  a file with this client's last transfer. Only the server's clock is involved,
  and only for equality, never for ordering. Nothing extra is stored on the
  server.
- Every server change is atomic: a single SFTP rename. Uploads go to a temp
  file first. The final rename never overwrites (plain SFTP `rename` fails if the
  target exists), so two uploads can never both land unnoticed.
- No locks, no ownership. The user decides on every collision.
- Folders are only namespaces: created on upload, removed when empty.

## Versions

A version of a file on the server is identified by its size and modification
time (mtime), as the server reports them in every listing and stat. SFTP v3
gives the mtime in whole seconds. Every upload creates a new file (a temp file
renamed into place), so the server sets a fresh mtime for every new version.
Renames, moves and claims keep the mtime, so they keep the version.

The transfer list records, for every download and upload, the SHA-256 of the
content and the server version (size and mtime) that was read or put in place.
A transfer list entry without a server version (written by SSH-Cloud before
v0.3) means "unknown version".

One second is coarse: two uploads within the same second with the same size
would get the same version. To prevent that, an upload that replaces a file
under the claim (see below) sets the mtime of its temp file to one second after
the claimed version's mtime when the server's own mtime is not later already.
Along the chain of versions at one path, uploaded through the app, the mtime
therefore always increases.

SSH-Cloud before v0.3 kept a hidden hash file `.<name>.sha256` next to every
file. These are no longer read or written. They stay hidden, a delete or move
in the app removes the one of that file (so an emptied folder can still be
removed), and `find <folder> -name '.*.sha256' -delete` on the server removes
them all.

## Workflow

**Download** (drag out, or double-click / "Download to..."):
open the file, stat the open handle, stream the content and compute SHA-256 from
the bytes actually read, then stat the handle again. The hash and the server
version are recorded in the transfer list. The open handle keeps reading the
version it opened, even when an upload replaces the file meanwhile. If the two
stats differ, or the bytes read do not match the size (the file was changed in
place by a tool outside the app), the version is recorded as unknown, so the next
upload prompts. A download never writes to the server.

**Upload** (drag back), per file:

1. If the local hash equals the latest transfer list entry and the server still
   has that entry's version: unchanged, skip.
2. Upload to a temp file `.<name>.tmp-<random>` next to the target, hashing while
   sending.
3. **Target does not exist**: rename temp to target. If
   the rename fails because the target now exists, someone else just created it:
   go to the prompt.
4. **Target exists**: claim it by renaming it to `.<name>.claim-<random>` with
   `posix-rename` (one atomic `rename(2)` on the server; the claim name is
   unique, so nothing is overwritten). Only one client can win this rename; a
   loser retries for a moment and then sees the winner's new version. (Plain
   SFTP rename is not used here: OpenSSH implements it as link + unlink, which
   is not exclusive for a contested source.)
   - The claimed file's size and mtime equal the transfer list entry: nobody
     else changed it. Give the temp file a later mtime if needed (see
     Versions), rename temp to target, delete the claim.
   - Otherwise (a different version, an unknown version, or never transferred
     by this client): rename the claim back and ask **Replace / Keep both /
     Skip** (option: apply to all).
   Even when nobody changed the server version, an upload from **another local
   file** with the same name asks first (checked before sending): only the
   local file of the last transfer replaces silently. That is the same path,
   or, for a drag out (recorded with a temp path, as Explorer does not say
   where it put the copy), a file with the same name.
5. Record the uploaded hash and the new server version (a stat of the temp file
   just before the final rename) in the transfer list.

Two rules keep step 3 and step 4 apart (a folder listing is not an atomic
snapshot, so a client can miss another client's claim and see the file as
missing while it is only claimed):

- An upload that started on an **existing** file never takes the new-file path.
  If the file is missing at commit time it was deleted, moved, or is claimed
  right now: prompt ("deleted, moved or being replaced during your upload").
- An upload that started as a **new** file never claims. If the file exists at
  commit time, someone created it meanwhile: prompt.

Why simultaneous uploads always prompt: only one client wins the claim, and the
loser then finds the winner's version, which has a later mtime than the one in
its transfer list. A new-file race is caught by the non-overwriting rename in
step 3. Both races are stress-tested with eight clients at once.

Why a crash is safe: the version is a property of the file itself, so there is
nothing to keep in step. A crash leaves at most a temp file or a claim file. A
claim file whose target is missing is renamed back by any client that sees it;
if the claimer is still busy, its own final rename fails and it prompts.

**Changes outside the app** (`scp`, a shell, another SFTP client) give the file
a new mtime, so they are seen as "changed on the server" like any other change.

**Replace** uses the same claim steps, without the comparison. **Keep both**
renames temp to `<name> (2).<ext>` (next free number).

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
- Entry: time, direction, remote path, local path, size, SHA-256, server mtime,
  result.
- Shown in a Transfers window, opened from the "..." menu (newest first,
  including moves and errors).
- Renames and moves made in the app update the paths in the list.

## Explorer window

- Windows 11 look: Fluent UI System Icons (MIT, embedded SVGs rendered at the
  screen's DPI) on flat native toolbars (their background painted white
  through TBSTYLE_CUSTOMERASE; Windows draws the buttons), the Explorer theme
  on the tree and list, Segoe UI Variable (the system font under Wine, which
  lacks it), a white content area with thin grey dividers, and a rounded
  window with a Mica title bar. The title is always "SSH-Cloud" and the window
  shows the app icon.
- Tabs in a grey band below the title bar, one per open connection, each with
  its own folder, history and listing. Drawn like Explorer's (GDI, double
  buffered): only the connection name, 200 px wide (up to 280 px for long
  names), rounded, the active tab white and joined to the row below, a close
  button on the active and the hovered tab, and a + button. New tab (+,
  Ctrl+T) opens the connection manager; close with Ctrl+W, a middle-click or
  the tab's right-click menu; Ctrl+Tab switches. Cut/copy and paste work
  within one connection.
- Navigation row: Back, Forward, Up, Refresh, and the address bar: a
  borderless edit in a slightly larger font, centred in a white box with a
  rounded grey border and an accent underline while it has focus.
- One command bar: New folder | Cut, Copy, Paste, Rename, Delete | Upload,
  Download | "..." (overflow menu: Transfers, Check for updates, then
  About ...). Icon-only buttons are 40 px wide with 8 px between them, like
  Explorer (the spacing is skipped under Wine, which lacks TB_SETMETRICS).
  Long operations are cancelled in their progress window; there is no Stop
  button.
- Folder tree (loaded as you expand it, kept in step with the list) and a file
  list with Name, Date modified, Type, Size and Status; Name takes the free
  width. The divider between them can be dragged (the width is remembered;
  double-click resets it). Real shell icons and type names; click a header to
  sort (folders first). Claim and temp files are hidden, and so are the
  `.<name>.sha256` hash files that SSH-Cloud before v0.3 wrote.
- A plain status line at the bottom, below a thin divider: status on the
  left, item count on the right.
- Status column: "Up to date" or "Changed on server" for files this client
  transferred before, from the size and mtime in the listing (no extra
  requests).
- Keyboard: Ctrl+X/C/V, Ctrl+A, Ctrl+Shift+N, F2, Del, F5, Backspace,
  Alt+Left/Right/Up, Ctrl+L or Alt+D for the address bar. While typing in the
  address bar or renaming, editing keys act on the text.
- Drag in from Windows Explorer: upload into the folder under the cursor or the
  current folder. Drag within the list onto a folder: move on the server.
- Drag out to Windows Explorer: a hand-written COM `IDataObject` offers
  `FileGroupDescriptorW` (built from listings only) and `FileContents`; each file
  is downloaded through the transfer engine only when the drop target asks for
  it, so the transfer list records its version. The first request downloads all
  files of the drag in the progress window (Cancel aborts the drop); later
  requests are answered from those downloads.
- Context menu: Open, Download to..., Cut, Copy, Paste, Rename, Delete,
  Copy path, New folder, Upload, Refresh.
- Server-side copy (Copy then Paste): `copy-data` on a small SFTP session of its
  own (pkg/sftp has no API for it), into a temp file and renamed into place
  without overwriting. Explorer style names ("report - Copy.docx"). The copy is
  a new file with its own version. Greyed out when the server has no
  `copy-data`.
- Uploads, downloads, copies and deletes run on a goroutine with a progress
  window (current file, bytes, speed, time left, Cancel). Collision prompts
  open on top of it.

## Updates

"Check for updates" in the "..." menu checks the latest GitHub
release on request. When it is newer, SSH-Cloud downloads the MSI in the
progress window, starts `msiexec` and closes. Nothing is checked
automatically and there is no setting for it. Development builds ("dev")
do not update.

## About

"About ..." (in the "..." menu) shows, all left aligned, the app icon
(rendered at the exact size, so it is sharp) and the name, version, a
one-line description, the author's copyright, "Released under the MIT
License", the GitHub repository (blue, opens the browser) and
the icon credits. It has no buttons; Esc, Enter or the close button close it.

## Logging

All packages log through `log/slog` to `%APPDATA%\SSH-Cloud\ssh-cloud.log`
(rotated at 5 MB): connects, login method and keys used (by fingerprint),
host key decisions, server capabilities, every move, delete, upload, download
and copy, conflicts and the user's choices, reconnects and
errors. Crashes and panics in background work are logged too. Passwords and
passphrases are never logged.

## Architecture

```
cmd/ssh-cloud/         the Windows program (manifest for common controls v6)
cmd/sshcloud-cli/      command-line tool for testing against a real server
internal/remote/       SSH/SFTP connection, login methods, Pageant, atomic ops,
                       folder create/prune, copy-data
internal/transfer/     download, upload, copy, versions, collision check, progress
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
   drag in (upload) and drag within (move), Transfers window.
5. Drag out to Windows Explorer (COM `IDataObject` with delayed rendering).
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
   multiple connections, and an update check.
10. Refinements from testing on Windows: one command bar with a "..."
    overflow menu, the address bar box, a draggable tree divider, the About
    window, progress for drag out, the upload races fixed under stress
    testing, and asking before replacing a file from another local file.

All milestones are done; the latest release is v0.1.9. Parts that need the
Windows UI (drag out, toolbars and theme, tabs, Pageant, the updater, the
About window) are only verified by testing on Windows and Wine.

## Known weaknesses (accepted)

- During an upload the file is missing from the server for a few milliseconds
  (between claim and final rename). A download in that moment fails and is
  retried.
- Servers on filesystems without hard links fall back to a stat-then-rename in
  `sftp-server`, which has a tiny race of its own.
- A version that does not come from an upload through the app can repeat a
  version this client recorded: a file deleted and created anew, or moved in,
  with the same size within the same second as the recorded version, or a tool
  that sets the mtime explicitly (`touch -d`, `rsync -t`, WinSCP keeping the
  local timestamp) to that same second with the same size. Such a change goes
  unnoticed.
- An upload of exactly the content that is already on the server is only
  skipped when this client transferred that version; otherwise it asks.
- The transfer list is per client: uploading from another machine always asks.
- A file that someone creates as new, at the exact moment another client holds
  a claim on an existing file with that name, can take the name. The claimed version is then kept as
  "name (2).ext" and that client is prompted; nothing is lost, but the version
  moves without its author being told.
- Pageant checks that the request's file mapping belongs to the current user;
  for an elevated (administrator) process this may fail, as the default owner
  is then the Administrators group.
