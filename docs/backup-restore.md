# Backup and restore

A backup is one file holding everything that makes a manager *this* manager:

- the database (devices, jobs, workflows, policy, audit log, revocations),
- the TLS certificate and **private key** every device has pinned,
- the operator token, AI key, remote-access settings and the pairing token,
- optionally the stored workload files.

Restore it on a new PC and the fleet reconnects without noticing: same
fingerprint, same tokens, same device records.

**A backup holds the TLS key and every token. Encrypt it before it leaves the
machine.** Files are written readable by their owner only (mode 0600; on
Windows an access list naming only you, checked with `icacls`).

## Making a backup

Running manager (the normal way; consistent, no downtime):

    harnessctl backup -to laptop.hhbk -include-artifacts -passphrase-file pass.txt

The passphrase can also come from `HARNESS_BACKUP_PASSPHRASE`. Without one the
backup is **not** encrypted and the command says so. Encryption is done by
`harnessctl`: the passphrase never reaches the manager. The manager snapshots
the database into a private temp file first (the read transaction ends before
anything is sent), so a slow client never holds up writes.

Stopped manager (offline, same format):

    manager backup -state-dir D:\harness\state -to laptop.hhbk -passphrase-file pass.txt

`-state-dir` is the installers' layout (`manager.db`, `tls\`, `operator-token`,
`ai-key`, `pairing-token`, `artifacts\`). Otherwise give the same `-db`,
`-tls-dir`, `-operator-token-file`, `-artifact-dir` flags the manager runs with.

Automatic: start the manager with

    -backup-dir D:\backups -backup-every 24h -backup-keep 7 -backup-passphrase-file D:\secret\pass.txt

Each backup is written to a hidden temp file, synced, renamed into place, then
**read back and verified**; one that fails its own check is deleted and the
older ones are kept. Only then are the oldest pruned. `harnessctl backup status`
shows the last result, disk health and artifact-store usage.

## Checking a backup

    manager restore -from laptop.hhbk -dry-run -passphrase-file pass.txt
    harnessctl backup verify laptop.hhbk -passphrase-file pass.txt

Checks the format version, every file's size and SHA-256 against the manifest,
that nothing unlisted is present, and that the database inside opens and passes
bbolt's structure check at a data-format version this manager understands.
Nothing on disk changes.

## Restoring

Stop the manager first. Restore refuses to run over a live manager (it tries
the database's exclusive lock) and refuses to replace an existing database
unless you say `-force`; anything replaced is kept as `<name>.pre-restore`.

    manager restore -from laptop.hhbk -state-dir D:\harness\state -passphrase-file pass.txt

Every file is staged beside its destination and verified before any is moved;
the database moves last. If it is interrupted, run it again with `-force`.
Then start the manager. Devices reconnect on their own.

A backup from a newer manager than the one you are running is refused with a
clear message (see "Data-format versions" below).

## The restore drill (do this once, before you need it)

1. `harnessctl backup -to drill.hhbk -passphrase-file pass.txt`
2. On a spare folder: `manager restore -from drill.hhbk -state-dir C:\drill -passphrase-file pass.txt`
3. Start a manager on spare ports from that folder; confirm its TLS fingerprint
   equals the live manager's, and `harnessctl nodes` lists your devices (offline).
4. Delete the drill folder and `drill.hhbk`.

Verified on this project's dev machine with the real binaries: backup taken
from a running manager, restored into a fresh folder, started on other ports,
same certificate fingerprint; restore over a running manager refused; wrong
passphrase refused; the encrypted file shows no plaintext; the file's access
list names only the owner.

## RPO and RTO

- **RPO** (data you can lose): everything since the last backup. With
  `-backup-every 24h`, up to a day of job history and device changes. Running
  work is not in a backup in any useful sense: devices drop it when the manager
  goes away. For a lower RPO shorten `-backup-every` (a backup of a small fleet
  takes well under a second) or run a standby manager (`harnessctl standby`),
  which copies state continuously.
- **RTO** (time to be back): copying the file plus `manager restore` is seconds;
  starting the manager and the fleet reconnecting is seconds more (measured:
  500 simulated devices back 7.9 s after a restart, 200 in 3.4 s). Budget
  **5-10 minutes** including finding the file and the passphrase.

## Data-format versions

The database records a data-format version. A manager upgrading an older
database first writes `manager.db.pre-upgrade-vN-to-vM-<time>.bak` (owner
only), then applies each step in its own transaction. A database from a
**newer** manager is refused untouched ("was written by a newer manager"), by
both the manager and `restore`. Format 1 is today's layout; no upgrade steps
exist yet. Delete the `.bak` once the upgrade has proved itself.

## File format (for the curious)

A zip with fixed entry names plus `backup-manifest.json` (format version, time,
data-format version, per-file size and SHA-256). Encrypted backups
(`.hhbk`) are `HHBKENC1`, the key-stretching count and salt, then 64 KiB
AES-256-GCM chunks (key from PBKDF2-HMAC-SHA256, 600,000 rounds; the standard
library has no scrypt/argon2). Each chunk authenticates its position and
whether it is the last, so reordering, truncation and header edits all fail.
Restore accepts only the known entry names; a path that tries to leave the
state folder is refused.
