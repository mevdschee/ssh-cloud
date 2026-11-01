# SSH-Cloud: Plan

A Windows app (Go + [windigo](https://github.com/rodrigocfd/windigo)) that shows an
SSH/SFTP server as an Explorer-like window. Browsing, renaming, moving and
deleting happen directly on the server. There is no sync and no editing in the
cloud: you **drag a file out** to work on it locally and **drag it back** to upload.
A local transfer list is used to detect collisions.

## Constraints

- Windows 10/11, native Win32 controls via windigo. Single `.exe`.
- Server: plain OpenSSH SFTP. No shell, no daemon.
- One shared SSH account, one key per person in `authorized_keys`. Key auth only.
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

Why simultaneous uploads always prompt: only one client wins the claim, and the
loser then finds a hash file with the winner's hash, which is not in its transfer
list. A new-file race is caught by the non-overwriting rename in step 3.

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
  "unknown version" overlay. A listing never writes anything.
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

## Transfer list

- Local, per connection profile, append-only (`%APPDATA%\SSH-Cloud\`).
- Entry: time, direction, remote path, local path, size, SHA-256, result.
- Shown in a Transfers window (history plus running transfers with progress).
- Renames and moves made in the app update the paths in the list.

## Explorer window

- Address bar, Back/Forward/Up, folder tree, file list (Name, Modified, Type,
  Size), status bar. Hash, claim and temp files are hidden.
- Overlay icon when a file changed on the server since your last transfer (hash
  files are read lazily, only for visible files that are in the transfer list).
- Shell icons via `SHGetFileInfo`, sorting, multi-select, usual keyboard
  shortcuts (F2, Del, F5, Backspace, Ctrl+X/C/V).
- Drag out: download (delayed rendering, so data only moves on drop).
  Drag in: upload with the check above. Drag within: server-side move.
- Context menu: Download to..., Rename, Delete, Copy path, Properties.
- Copy only server side (`copy-data` extension); greyed out if unsupported.
- Long operations run in the background with progress and cancel.

## Architecture

```
internal/remote/       SSH/SFTP connection, atomic ops, folder create/prune
internal/transfer/     download, upload, hashing, collision check
internal/transferlog/  local transfer list
internal/ui/           windigo windows, list/tree, dialogs, drag and drop
```

Everything outside `ui` is testable on Linux against a local `sshd`.

## Milestones

1. Remote layer: connect, list, rename, move, delete.
2. Transfers: download, atomic upload, transfer list, collision check.
3. Explorer window with download/upload buttons and the collision prompt.
4. Drag and drop, Transfers window.
5. Polish: tree, overlays, server-side copy, shortcuts.

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
