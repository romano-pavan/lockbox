# lockbox

Immutable offsite backups of a MySQL or MariaDB database on Amazon S3, in one command.
[![CI](https://github.com/romano-pavan/lockbox/actions/workflows/ci.yml/badge.svg)](https://github.com/romano-pavan/lockbox/actions/workflows/ci.yml)


## In short

If you just need to know whether this tool fits your problem, read this section and skip the rest.

**What it does.** Takes a full logical dump of a MySQL or MariaDB database, compresses it, uploads it to an Amazon S3 bucket, and verifies it arrived. Nothing else.

**How it protects the copy.** The bucket has S3 Object Lock switched on. Every uploaded file gets a retention date. Until that date passes, the file cannot be deleted or overwritten by anyone, including the account owner. On top of that, the credentials stored on the database server are allowed to upload and nothing else. They cannot delete, and they cannot read older backups either. Public access to the bucket is blocked and the data is encrypted at rest by Amazon.

**What you need.** An Amazon Web Services account, and one administrator access key that is used for about a minute during setup and then deleted. The database server needs the `mariadb-client` package and this one binary.

**Full setup, three commands:**

```bash
export AWS_ACCESS_KEY_ID=AKIA...        # administrator key, used once
export AWS_SECRET_ACCESS_KEY=...
sudo -E lockbox init --bucket your-unique-name --database yourdb --days 30 --mode COMPLIANCE
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
```

**Every backup after that, one command:**

```bash
lockbox backup
```

**Restoring, with an administrator key on any machine:**

```bash
lockbox restore --list
lockbox restore 2026-09-03T13-17-35Z
```

**Scheduling.** Copy the two files from `systemd/` and enable the timer, or call `lockbox backup` from anything that can run a command. Once a night, three times a day, every hour, whatever you need. The tool keeps no state between runs.

**Cost.** `lockbox init` prints an estimate before it creates anything. Read the section on cost below so the number does not surprise you later.

**Limits worth knowing before you start.** The compressed dump must stay under 5 GiB. Every backup is a full dump, so there is no incremental mode. The data is encrypted by Amazon, not by you, so Amazon holds those keys. Amazon S3 is the only tested target.

Everything below is the detail: why it is built this way, what the alternatives do wrong, how to prove the protection works, and what to do when the server is gone.


## Tested against

A Debian 13 virtual machine on Proxmox, MariaDB 11.8, with the [test_db `employees` dataset](https://github.com/datacharmer/test_db): 147 MiB of table data across 8 tables, 300 024 employee rows, 2 844 047 salary rows.

| Step | Result |
|---|---|
| Compressed dump | 47.4 MiB |
| Backup, dump to verified upload | about 5 seconds |
| Restore, download to loaded database | 17.5 seconds |
| Row counts after restore | identical |
| Monthly storage, one copy, STANDARD_IA, eu-central-1 | under one US cent |

The restore ran on a rolled back snapshot with no database present, using an administrator key, the way a real recovery would go.


## The problem

A small company runs its accounting or resource planning database on one Linux server. Backups go to a network share on a storage device in the same building.

Ransomware encrypts everything it can mount, and that share is mounted. Once an administrator account is taken over, the backups die with the production data. Insurers increasingly ask for proof of an offsite copy that cannot be altered.

## What lockbox does about it

```
Database server (Debian, MariaDB)
        │
        │  mariadb-dump --single-transaction  →  gzip  →  SHA-256 and MD5
        │  upload with a key that has PutObject and nothing else
        ▼
Amazon S3 bucket
        versioning · server side encryption · public access blocked
        Object Lock, COMPLIANCE mode, retention set at bucket level
```

Four controls carry the design:

| Control | What it stops |
|---|---|
| Object storage instead of a network share | The bucket cannot be mounted, so ransomware never sees it as a drive |
| Upload only credentials | The key on the server cannot call `DeleteObject`, and cannot call `GetObject` either |
| Object Lock, COMPLIANCE mode | For the retention period nobody deletes an object. Not an administrator, not the account root user, not Amazon support |
| Administrator key used once, then discarded | The credentials that could undo any of the above never live on the database server |

Object Lock is the only hard guarantee in that list. The rest are permission policies, and policies bend for whoever holds enough privilege. That is why `lockbox doctor` finishes by asking Amazon to delete something and treats the refusal as the passing result.

## Why this exists

Every existing tool stops short somewhere. Checked against primary sources in September 2026. If something has changed since, open an issue and I will correct it.

**Proxmox Backup Server** supports S3 compatible object storage as a datastore backend, stable since 4.2. It does not do Object Lock. A Proxmox staff member states on the official forum that object locking is not part of the S3 implementation and that deduplication makes it hard to add. Community threads give the concrete reason: garbage collection and pruning issue deletes, and a chunk of deduplicated data can have its lock expire while a newer backup still references it. Third party guidance for 4.2 goes further and warns that enabling Object Lock on a bucket PBS uses can corrupt the datastore structure. That last claim is not Proxmox's own, so treat it as a strong caution rather than doctrine. Either way, PBS plus Object Lock is not a supported combination today.

**Kopia** does support Object Lock, and its documentation recommends compliance mode. Two caveats. It does not renew retention dates unless you enable lock extension in full maintenance, and you must then run full maintenance more often than the retention period or the locks quietly expire. More seriously, it keeps immutable backup content and mutable internal metadata in the same bucket, so its runtime credentials need `s3:PutObjectRetention`. An [open issue from March 2026](https://github.com/kopia/kopia/issues/5199) argues that this hands any compromised credential the power to extend or defeat locks on the bucket holding the backups, so credential isolation is not achieved. Kopia also needs `s3:DeleteObject` to write delete markers.

**restic** has no Object Lock support. The feature request, [issue #4992](https://github.com/restic/restic/issues/4992), has been open since August 2024. Earlier, in [issue #1544](https://github.com/restic/restic/issues/1544), a maintainer declined append-only support and a third party maintains a `restic-worm` fork instead. restic needs delete permission for its own lock files, so the write-only credential model this project is built on does not fit it.

**The various `mysql-backup-s3` scripts** upload a dump and stop. Creating the bucket, turning on Object Lock and writing a least privilege policy are left to you, which is the part most people get wrong.

lockbox fills that gap and does nothing else. For whole virtual machines use Proxmox Backup Server. The two are complementary.

## Install

Download a binary from the releases page, or build it:

```bash
git clone https://github.com/romano-pavan/lockbox
cd lockbox
go build -o lockbox ./cmd/lockbox
sudo install -m 0755 lockbox /usr/local/bin/lockbox
```

It needs the MariaDB or MySQL client programs, which a database server already has:

```bash
sudo apt install mariadb-client
```

## First run

You need an Amazon Web Services account and one administrator access key, created in the web console. That key works for about a minute and is then thrown away.

```bash
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...

sudo -E lockbox init \
  --bucket your-globally-unique-name \
  --region eu-central-1 \
  --database erp \
  --days 1 \
  --mode GOVERNANCE
```

Start with one day and GOVERNANCE. You will want to delete the test objects tomorrow, and COMPLIANCE will not let you. Move to COMPLIANCE and a real retention once a restore has actually worked.

`init` prints its plan, including a rough monthly cost, and waits for confirmation. It then creates the bucket with Object Lock enabled, turns on versioning, server side encryption and a block on all public access, sets a bucket wide default retention so uploads are locked without the client needing permission to set locks, adds a lifecycle rule that expires objects once their lock has run out, creates an Identity and Access Management user whose policy allows uploading and explicitly denies deleting, reading and weakening, and writes that user's access key to `/etc/lockbox/credentials` with mode 0600.

```bash
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
time lockbox backup
lockbox doctor
```

`unset` matters. lockbox reads the environment before it reads `/etc/lockbox/credentials`, so an administrator key left exported means the backup runs with it, and `doctor` correctly complains that these credentials can delete.

`time` prints how long a command took. Note the figure for your first backup and your first restore. Those two numbers are your real recovery expectations and they belong in your own documentation, not in mine.

## What it costs

`lockbox init` prints an estimate before it creates anything. Read it as a floor, not a bill.

The estimate covers **one backup**, then multiplies by the retention in days assuming one backup per day. Each run creates a separate object with its own storage charge, and every object is billed for as long as the retention keeps it. Backing up three times a day triples the number of stored objects and triples that line of the bill.

The figure uses STANDARD_IA rates, the storage class lockbox uploads to. It ignores request charges, which are trivial at this volume, and egress, which you only pay during a restore, at roughly nine US cents per gigabyte.

For real numbers, watch the Amazon console under Billing and Cost Management, and set a Budget with an email alert. That takes two minutes and it is the only figure that is actually yours.

## Proof

Five captures tell the story. The third is the one that matters.

**1. `lockbox init`**, the plan, the confirmation, the list of things created. See `docs/init.png`.

**2. `lockbox backup`**, one line, with the date until which the object cannot be deleted:

```
Dumping employees ...
Uploading 47.4 MiB to s3://lockbox-test-rp-20260904/lockbox/db01/2026-09-03T13-17-35Z.sql.gz ...
OK lockbox/db01/2026-09-03T13-17-35Z.sql.gz (47.4 MiB, sha256 ...), locked in GOVERNANCE mode until 2026-09-04 13:17 UTC
```

**3. `lockbox doctor`**, the two lines that read *refused*. See `docs/doctor.png`.

```
Credentials
  ✓ loaded from /etc/lockbox/credentials
  ✓ identity arn:aws:iam::...:user/lockbox/lockbox-test-rp-20260904-writer

Storage
  ✓ bucket lockbox-test-rp-20260904 is reachable
  ✓ Object Lock active: GOVERNANCE mode, 1 day

Backups
  ✓ 1 backup stored, newest 1m0s old (47.4 MiB)
  ✓ reading stored backups refused, as intended

Protection
  ✓ delete refused by Amazon, as intended
```

The backup server holds working credentials, can prove the bucket is locked, and is refused when it tries to delete or read. That is the whole design, verified from the machine an attacker would land on.

**4. An administrator failing to delete a locked object.** Run this with a key that has full permissions, so no policy stands in the way and only Object Lock is left:

```bash
aws s3api list-object-versions --bucket YOUR-BUCKET \
  --query 'Versions[0].[Key,VersionId]' --output text
aws s3api delete-object --bucket YOUR-BUCKET --key KEY --version-id VERSION
```

The refusal cites WORM protection. A full administrator unable to erase a backup is the strongest single piece of evidence here.

**5. Restore on a machine with no database.** See `docs/restore.png`.

```
$ lockbox restore --list
Backups in s3://lockbox-test-rp-20260904/lockbox/db01/

  2026-09-03T13-17-35Z        47.4 MiB  2026-09-03 15:17

$ time lockbox restore 2026-09-03T13-17-35Z
Restoring lockbox/db01/2026-09-03T13-17-35Z.sql.gz ...
Restore finished. Check row counts against what you expect before trusting it.

real    0m17.561s
```

## Schedule it

```bash
sudo cp systemd/lockbox-backup.service systemd/lockbox-backup.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now lockbox-backup.timer
systemctl list-timers lockbox-backup.timer
```

The command exits non-zero on any failure, so a failed unit is a real signal you can hang monitoring on.

Run it as often as you like. lockbox keeps no state between runs and holds no locks of its own. Every invocation is an independent dump, upload and verification. The shipped timer fires once a night because that suits most small installations, but twice a day or every four hours is equally valid: edit `OnCalendar` in the timer, or call `lockbox backup` from anything that can run a command. Two things scale with frequency. The storage bill, since every run is a separate locked object. And the staleness threshold, so pass a matching `--max-age` to `doctor`:

```bash
lockbox doctor --max-age 5h
```

## Restore

Restores use an administrator key, not the one on the database server. That key deliberately cannot read what it wrote.

```bash
lockbox restore --list
lockbox restore 2026-09-03T02-30-11Z
lockbox restore --to-file /tmp/dump.sql 2026-09-03T02-30-11Z   # inspect instead of loading
```

Do this on a spare machine before you trust any of it, and write down the elapsed time. That number is your recovery time. An untested backup is a rumour.

## If the server is gone

Nothing about recovery depends on this tool, on the configuration file, or on anything stored on the machine that died. What sits in the bucket is an ordinary gzip compressed text dump, byte for byte what `mariadb-dump` produces. No proprietary format, no chunk index, no separate metadata store, no key that only lockbox holds.

That is deliberate, and it is the main reason this design uses a plain dump instead of a deduplicating engine. With chunk based tools the data only means something through the tool and its index. Lose either and you have gigabytes of unusable blocks. Here you have a text file and `gunzip`.

To recover from nothing at all you need three facts and access to the Amazon account. None of them live on the database server.

| What | Where to keep it |
|---|---|
| Amazon account access | Root sign in with multi factor authentication, off site |
| Bucket name and region | Written into your own runbook, not on the server |
| The three commands below | This page |

```bash
apt install -y mariadb-server awscli

aws s3 ls s3://YOUR-BUCKET/lockbox/YOUR-HOST/

aws s3 cp s3://YOUR-BUCKET/lockbox/YOUR-HOST/2026-09-03T13-17-35Z.sql.gz - \
  | gunzip | mariadb
```

Write those three lines, your bucket name and your region onto one page, and keep that page somewhere the server cannot take down with it. A password manager, a printout in a safe, a document at a different company. That page is the recovery plan. lockbox is a convenience on top of it.

## Going to production

The quick start above is a lab exercise. Five things change for real use.

Put the bucket in **a separate Amazon account**. Otherwise an attacker who takes over the production account still cannot delete existing backups, thanks to COMPLIANCE mode, but can stop new ones from being written. Never let production credentials administer the backup account.

Use **COMPLIANCE mode with a real retention**. GOVERNANCE bends for anyone holding `s3:BypassGovernanceRetention`. Prove the flow on a small bucket with one day first, then create the production bucket with the retention you actually want. A retention already applied cannot be shortened.

Add **an alarm for the backup that does not happen**. A tool that fails silently is indistinguishable from one that was never installed. Hang an `OnFailure` hook on the systemd unit, or a CloudWatch alarm that fires when no new object has appeared inside your interval. A compromised server must not be able to publish those notifications itself, or it can drown you in noise.

Keep **a read only key off the server** so a restore does not need an administrator. Give it `s3:GetObject` and `s3:ListBucket` on the bucket and nothing else, and store it in a password manager rather than on any machine.

Run **a restore drill every three months** and write down the elapsed time. The only number management actually cares about is how long recovery takes.

## Configuration

`lockbox init` writes `/etc/lockbox/config.yml`. Access keys are never stored in it.

```yaml
database:
  type: mariadb        # mariadb or mysql
  host: localhost      # a local host uses socket authentication, so no password anywhere
  port: 3306
  name: erp            # empty means every database
  defaults_file:       # my.cnf style file, only needed for a remote database

storage:
  bucket: acme-erp-backup-2026
  region: eu-central-1
  prefix: lockbox
  endpoint:            # empty for Amazon S3, set for S3 compatible storage

retention:
  days: 30
  mode: COMPLIANCE
```

Credentials are read from `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, then `/etc/lockbox/credentials`, then `~/.aws/credentials`.

Editing this file afterwards changes nothing in Amazon. The retention lives on the bucket, not here. Raise `days` in the file and `doctor` reports the mismatch, which is the point of that check. `database`, `prefix`, `host`, `port` and `defaults_file` are safe to change at any time.

## Design notes

**Object Lock is not a one shot decision at bucket creation.** It used to be. Until November 2023 you had to contact AWS Support to add it to an existing bucket. Today `PutObjectLockConfiguration` turns it on for any versioned general purpose bucket, and the console offers it under Properties. lockbox still enables it at creation because that is the only moment the bucket is empty. Turning it on later does not retroactively lock what is already stored; for that you need S3 Batch Operations against an inventory report. Three related facts from the Amazon documentation before you commit: Object Lock requires versioning, once enabled it cannot be disabled and versioning cannot be suspended, and under compliance mode the only way to delete an object before its retention expires is to close the AWS account.

**Why the retention sits on the bucket, not on each object.** A bucket wide default retention applies to every upload automatically, so the uploading key never needs `s3:PutObjectRetention`. Given that permission it could also manipulate locks, which is exactly the hole this design closes and the one Kopia currently leaves open.

**Why verification uses ListObjectsV2 rather than HeadObject.** `HeadObject` is authorised as a read of the object and requires `s3:GetObject`, which the upload key does not have. Listing works, and comparing the stored size against the local size proves the object arrived intact.

**Why the dump is written to disk before uploading.** The size and both digests of the finished object have to be known before the request can be signed. Building the file first also means a dump that fails halfway never becomes a locked object that cannot be removed for a month.

**Why Content-MD5 goes on every upload.** A bucket with Object Lock enabled rejects `PutObject` without a checksum header. restic hit the same wall in [issue #2202](https://github.com/restic/restic/issues/2202) back in 2019. lockbox computes SHA-256 and MD5 in the same pass over the compressed stream, so the file is hashed once and sent once.

**Why `--single-transaction`.** It takes a consistent snapshot inside one transaction, so applications keep reading and writing while the dump runs. Stopping the database is never necessary for InnoDB. `doctor` warns when it finds MyISAM or Aria tables, because that guarantee does not cover them.

**Why no external libraries.** Signature Version 4 is a few hundred lines of standard hashing. Avoiding the vendor software development kit keeps the binary small, makes the program auditable in an afternoon, and means no dependency updates for a tool that should keep working untouched for years. The signing code is checked against Amazon's published example in `go test ./...`.

## When something goes wrong

| Message | Cause |
|---|---|
| `configuration file ... not found` | `init` has not run, or you are not the user that owns `/etc/lockbox` |
| `these credentials ARE allowed to delete objects` | The administrator key is still exported. Run `unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY` |
| `Content-MD5 ... is required` | You are running a build older than the checksum fix |
| `BucketAlreadyExists` | The name is taken by another Amazon customer. Bucket names are global |
| `bucket already exists in this account, reusing it` | Not an error. `init` is safe to repeat after a failure part way through |
| `may not read backups; use an administrator key` | Expected during a restore. The upload key cannot read, by design |
| `neither mariadb-dump nor mysqldump is installed` | `apt install mariadb-client` |
| `Unable to locate credentials` from the `aws` tool | The `aws` tool does not read `/etc/lockbox/credentials`. Export an administrator key for that command |
| `aws: command not found` as root | The `aws` tool is installed for another user. `lockbox init` prints the account and identity anyway, so that check is optional |

## Limitations

Single upload only, so the compressed dump must stay under 5 GiB. Larger databases need multipart upload, which is not implemented.

Logical dumps only. For very large databases a physical hot backup tool is the better fit.

Every backup is a full dump. There is no incremental mode, so the recovery point objective is however often you schedule it, and the storage bill scales with frequency times retention. Point in time recovery would need binary logs.

No client side encryption. Data is encrypted in transit and at rest by Amazon, but Amazon holds those keys.

Amazon S3 only. S3 compatible storage can be pointed at with `endpoint`, but not every provider implements Object Lock the same way, and some do not implement it at all.

## Development

```bash
go test ./...     # includes signature checks against Amazon's published example
go vet ./...
gofmt -l .
```

Cross compiling needs no toolchain:

```bash
GOOS=linux GOARCH=arm64 go build -o lockbox-arm64 ./cmd/lockbox
```

## A note on how this was built

This tool was written with substantial help from an AI assistant (Claude), disclosed the same way as in [sitecheck](https://github.com/romano-pavan/sitecheck). The architecture decisions, the threat model, the research into what existing tools do and do not cover, and the testing against a real database are mine. The Go idioms and much of the drafting are not. Worth stating plainly rather than leaving people to guess.

## License

MIT
