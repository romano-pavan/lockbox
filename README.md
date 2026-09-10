# lockbox

Immutable offsite backups of a MySQL or MariaDB database on Amazon S3, in one command.

> **Hrvatski.** Alat koji radi kopiju baze podataka i šalje je u Amazonov oblak tako da je poslije **nitko ne može obrisati ni promijeniti**, ni ti sam. To je zaštita od ransomwarea: ako netko provali na server i sve enkriptira, kopija u oblaku ostaje netaknuta jer je server fizički nema pravo dirati.
>
> Cijeli README ima objašnjenja na hrvatskom u ovakvim okvirima. Engleski dio je ono što vide ljudi na GitHubu, hrvatski je za tebe i za svakoga tko želi znati što neka rečenica tehnički znači.

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

> **Hrvatski.** Ovo je sažetak za nekoga tko traži alat i nema volje čitati deset stranica. Bitne stvari: radi običan dump baze, šalje ga u S3, zaključa ga na X dana. Za postavljanje treba admin ključ jednom, poslije server ima ključ koji smije samo slati. Ograničenja: do 5 GB sažete kopije, uvijek puna kopija (nema inkrementalnog), enkripcija je Amazonova a ne tvoja.

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

> **Hrvatski.** Ovo nisu teoretske brojke. Baza je stvarno kopirana, virtualka je vraćena na stanje prije nego je baza uopće postojala, i baza je vraćena iz oblaka. Broj redaka nakon vraćanja je bio identičan. `47.4 MiB` je koliko je 147 megabajta baze zauzelo nakon sažimanja.

## The problem

A small company runs its accounting or resource planning database on one Linux server. Backups go to a network share on a storage device in the same building.

Ransomware encrypts everything it can mount, and that share is mounted. Once an administrator account is taken over, the backups die with the production data. Insurers increasingly ask for proof of an offsite copy that cannot be altered.

> **Hrvatski.** Klasičan scenarij: firma ima bazu na jednom serveru i backup na mrežnom disku u istoj sobi. Ransomware enkriptira sve što vidi kao disk, a mrežni disk je montiran, dakle vidi ga. Kad napadač dobije administratorski račun, briše i backupe. Osiguravajuće kuće sve češće traže dokaz da postoji kopija izvan lokacije koja se ne može mijenjati.

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

> **Hrvatski, pojam po pojam.**
>
> **Object storage.** S3 nije disk. Nema slova diska, ne montira se u sustav, pristupa mu se isključivo preko web sučelja s naredbama tipa „pošalji objekt", „izlistaj objekte". Ransomware pretražuje montirane diskove i mrežne dijeljene mape. S3 spremnik u toj pretrazi ne postoji.
>
> **Object Lock.** Amazonova značajka koja objektu doda datum do kojeg se ne smije obrisati ni prepisati. To nije dozvola koju netko može promijeniti, nego pravilo koje Amazon provodi u samoj pohrani.
>
> **COMPLIANCE nasuprot GOVERNANCE.** Dva načina zaključavanja. GOVERNANCE se može zaobići ako netko ima posebnu dozvolu `s3:BypassGovernanceRetention`, dakle štiti od greške ali ne od napadača s dovoljno prava. COMPLIANCE se ne može zaobići nikako. Jedini način da obrišeš objekt prije isteka je zatvoriti cijeli Amazon račun.
>
> **PutObject, GetObject, DeleteObject.** Imena operacija u S3 sučelju: pošalji objekt, pročitaj objekt, obriši objekt. Ključ na serveru smije samo prvu. Ne smije brisati, ali ni čitati, jer napadač koji ukrade ključ inače može pokupiti sve stare kopije i ucjenjivati te objavom podataka.
>
> **Zašto je Object Lock jedina tvrda garancija.** Sve ostalo su IAM politike, a politiku može promijeniti tko god ima dovoljno prava u računu. Object Lock ne može promijeniti nitko.

## Why this exists

Every existing tool stops short somewhere. Checked against primary sources in September 2026. If something has changed since, open an issue and I will correct it.

**Proxmox Backup Server** supports S3 compatible object storage as a datastore backend, stable since 4.2. It does not do Object Lock. A Proxmox staff member states on the official forum that object locking is not part of the S3 implementation and that deduplication makes it hard to add. Community threads give the concrete reason: garbage collection and pruning issue deletes, and a chunk of deduplicated data can have its lock expire while a newer backup still references it. Third party guidance for 4.2 goes further and warns that enabling Object Lock on a bucket PBS uses can corrupt the datastore structure. That last claim is not Proxmox's own, so treat it as a strong caution rather than doctrine. Either way, PBS plus Object Lock is not a supported combination today.

> **Hrvatski.** Proxmox Backup Server od verzije 4.2 zna slati backupe u S3, ali ne zna raditi s Object Lockom. Zašto to nije sitnica:
>
> **Deduplikacija** znači da alat ne sprema svaku kopiju cijelu. Datoteke razlomi na komadiće (chunks), izračuna otisak svakog komadića, i sprema samo one koje još nema. Ako se ista tablica nije mijenjala tjedan dana, tih komadića ima **jedan primjerak** koji dijeli svih sedam dnevnih backupa. Zato PBS zauzima puno manje prostora nego sedam punih dumpova.
>
> **Garbage collection i prune.** Kad obrišeš stari backup, komadići koje više nitko ne koristi moraju se počistiti, inače spremište raste zauvijek. Taj posao radi „garbage collection", a `prune` je brisanje starih točaka po pravilu tipa „drži zadnjih 7 dnevnih, 4 tjedna, 12 mjeseci". Oba postupka **brišu** podatke iz spremišta.
>
> **Zašto se to sudara s Object Lockom.** Object Lock zabranjuje brisanje. Alat koji mora redovito brisati da bi uopće funkcionirao ne može raditi u spremniku koji brisanje zabranjuje. Zato PBS-u treba dozvola za brisanje, a ta ista dozvola je ono što napadaču treba.
>
> **Konkretna rupa koju su korisnici opisali.** Zamisli da je komadić podataka napisan prije 40 dana, a zaključavanje traje 30 dana. Zaključavanje mu je isteklo. Ali taj isti komadić je i dalje dio jučerašnjeg backupa, jer se ta tablica nije mijenjala. Napadač ga sada može obrisati i time pokvariti i najnoviji backup, iako je najnoviji backup formalno „zaključan". Kod običnog dumpa tog problema nema jer svaka kopija stoji sama za sebe.

**Kopia** does support Object Lock, and its documentation recommends compliance mode. Two caveats. It does not renew retention dates unless you enable lock extension in full maintenance, and you must then run full maintenance more often than the retention period or the locks quietly expire. More seriously, it keeps immutable backup content and mutable internal metadata in the same bucket, so its runtime credentials need `s3:PutObjectRetention`. An [open issue from March 2026](https://github.com/kopia/kopia/issues/5199) argues that this hands any compromised credential the power to extend or defeat locks on the bucket holding the backups, so credential isolation is not achieved. Kopia also needs `s3:DeleteObject` to write delete markers.

> **Hrvatski.** Kopia je besplatan alat s grafičkim sučeljem koji Object Lock podržava, ali s dvije zamke.
>
> **Renew retention dates, produljivanje datuma zaključavanja.** Kopia isto deduplicira. Komadić napisan prvog u mjesecu ima zaključavanje do tridesetog. Ali ako se podaci u međuvremenu nisu mijenjali, taj isti komadić trideset i prvog još uvijek treba, a zaključavanje mu je isteklo. Rješenje je produljiti mu datum, dakle svakom komadiću koji je još u upotrebi pomaknuti rok naprijed. Kopia to **ne radi sama od sebe**, moraš uključiti opciju.
>
> **Full maintenance, potpuno održavanje.** Kopia ima dvije razine čišćenja. „Quick" radi sitne stvari često. „Full" prolazi kroz cijelo spremište, gleda što se još koristi, briše nekorišteno i, ako si to uključio, produljuje zaključavanja. Taj puni prolaz **moraš pokretati češće nego što traje zaključavanje**. Ako ti je zaključavanje 30 dana a puno održavanje se zadnji put izvršilo prije 35 dana, dio tvojih backupa više nije zaključan i ti to ne znaš. Odatle „quietly expire", tiho isteknu, bez ijedne poruke.
>
> **Mutable metadata, promjenjivi metapodaci.** Kopia u istom spremniku drži i podatke (koji se ne mijenjaju) i vlastitu evidenciju: popis komadića, indekse, oznake koji backup sadrži što. Ta evidencija se **mora** mijenjati pri svakom backupu.
>
> **`s3:PutObjectRetention`.** Dozvola za postavljanje i produljivanje datuma zaključavanja. Kopia je treba jer sama upravlja rokovima. Problem: ako netko ukrade kredencijale sa servera, dobio je i tu dozvolu. Ne može skratiti COMPLIANCE rok, ali može petljati po evidenciji i po rokovima drugih objekata, pa izolacija kredencijala zapravo ne postoji. To je bit otvorenog prigovora na GitHubu.
>
> **Delete markers, oznake brisanja.** Kad je uključeno versioning i netko „obriše" objekt bez navođenja verzije, S3 ne briše ništa nego stavi oznaku „ovo se smatra obrisanim". Stara verzija ostaje. Kopia treba dozvolu `s3:DeleteObject` da bi mogla postavljati te oznake. lockbox tu dozvolu nema uopće.
>
> **Zašto lockbox nema taj problem.** Rok zaključavanja je postavljen na razini spremnika, jednom, i Amazon ga primjenjuje sam na svaki novi objekt. Ključ na serveru ne treba nikakvu dozvolu za rokove, pa je ni nema.

**restic** has no Object Lock support. The feature request, [issue #4992](https://github.com/restic/restic/issues/4992), has been open since August 2024. Earlier, in [issue #1544](https://github.com/restic/restic/issues/1544), a maintainer declined append-only support and a third party maintains a `restic-worm` fork instead. restic needs delete permission for its own lock files, so the write-only credential model this project is built on does not fit it.

> **Hrvatski.** restic je vrlo popularan alat za backup, ali Object Lock jednostavno nema.
>
> **Append-only, samo dodavanje.** Način rada u kojem se smije samo dopisivati, nikad brisati ni mijenjati. Netko je to tražio od restica još 2018., održavatelj je odbio, pa je treća strana napravila vlastitu preinaku pod imenom `restic-worm`. WORM znači write once read many, napiši jednom čitaj mnogo puta.
>
> **Vlastite lock datoteke.** Restic pri radu stvara privremenu datoteku koja govori „ja trenutno radim, ne dirajte spremište", i na kraju je briše. Da bi je obrisao, treba mu dozvola za brisanje. Time pada cijela zamisao ključa koji ne smije brisati. To nije nemarnost nego posljedica arhitekture: alat koji upravlja vlastitim spremištem mora ga smjeti mijenjati.

**Duplicati** has no native Object Lock support either. The feature request has been open since 2020, and users on the project forum were still reporting its absence in late 2024.

> **Hrvatski.** Duplicati je besplatan alat s web sučeljem, popularan za kućnu upotrebu. Zahtjev za Object Lock podrškom otvoren je 2020. i krajem 2024. korisnici su na forumu još pisali da toga nema. Object Lock možeš uključiti na spremniku sam, ali Duplicati o tome ne zna ništa, pa će mu vlastito čišćenje starih verzija pucati na odbijenice.

**The various `mysql-backup-s3` scripts** upload a dump and stop. Creating the bucket, turning on Object Lock and writing a least privilege policy are left to you, which is the part most people get wrong.

> **Hrvatski.** Na GitHubu postoji desetak skripti koje rade dump i pošalju ga u S3. Sve staju na tome. Napraviti spremnik, uključiti Object Lock, versioning, blokirati javni pristup i napisati IAM politiku koja dozvoljava samo slanje, to ostaje na tebi. **Least privilege** znači davanje najmanjeg skupa dozvola koji je dovoljan za posao, i upravo tu većina zaluta, jer je najlakše dati ključu potpune ovlasti nad spremnikom.

lockbox fills that gap and does nothing else. For whole virtual machines use Proxmox Backup Server. The two are complementary.

> **Hrvatski.** lockbox štiti bazu, Proxmox Backup Server štiti cijele virtualke. To se ne isključuje, koristi oboje.

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

> **Hrvatski.** `go build` napravi jednu izvršnu datoteku. Nema instalacije, nema ovisnosti, nema servisa u pozadini. `install -m 0755` je kopiranje s postavljanjem prava na „vlasnik smije pisati, svi smiju čitati i pokretati". `mariadb-client` daje programe `mariadb-dump` i `mariadb`, koje lockbox poziva.

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

> **Hrvatski, pojam po pojam.**
>
> **Access key.** Par vrijednosti, javni identifikator koji počinje s `AKIA` i tajni dio. To je korisničko ime i lozinka za Amazonovo programsko sučelje. Tajni dio se prikazuje samo jednom, pri izradi.
>
> **`export`.** Postavlja varijablu okoline. Živi samo u tom prozoru terminala i nigdje se ne zapisuje na disk. Zatvoriš prozor, nestala je. Zato je to sigurnije mjesto za admin ključ od datoteke.
>
> **`sudo -E`.** `sudo` pokreće kao root. Zastavica `-E` prenosi tvoje varijable okoline u tu naredbu. Bez nje root ne bi vidio ključ koji si upravo izvezao.
>
> **`unset`.** Briše varijablu iz okoline. Ovo je važan korak, ne kozmetika. lockbox traži kredencijale prvo u okolini, pa tek onda u `/etc/lockbox/credentials`. Ako admin ključ ostane izvezen, backup će se izvršiti njime, a `doctor` će ti javiti da ovi kredencijali smiju brisati, što je točno i što je problem.
>
> **Versioning.** Amazonova značajka koja pri prepisivanju objekta čuva staru verziju umjesto da je izgubi. Object Lock ga zahtijeva, jer bez povijesti verzija nema što štititi.
>
> **Server side encryption.** Amazon šifrira podatke na svojim diskovima. Podaci su zaštićeni od nekoga tko bi fizički došao do diska u podatkovnom centru, ali ključeve drži Amazon, ne ti.
>
> **Lifecycle rule.** Pravilo koje automatski briše objekte starije od zadanog broja dana, da ne plaćaš vječno. lockbox ga postavi na rok zaključavanja plus pet dana, jer prije isteka zaključavanja Amazon ionako neće brisati.
>
> **IAM.** Identity and Access Management, Amazonov sustav korisnika i dozvola. lockbox u njemu napravi zasebnog korisnika samo za slanje backupa.
>
> **Prava 0600.** Datoteku smije čitati i pisati samo vlasnik, dakle root. Nitko drugi na sustavu je ne vidi.

## What it costs

`lockbox init` prints an estimate before it creates anything. Read it as a floor, not a bill.

The estimate covers **one backup**, then multiplies by the retention in days assuming one backup per day. Each run creates a separate object with its own storage charge, and every object is billed for as long as the retention keeps it. Backing up three times a day triples the number of stored objects and triples that line of the bill.

The figure uses STANDARD_IA rates, the storage class lockbox uploads to. It ignores request charges, which are trivial at this volume, and egress, which you only pay during a restore, at roughly nine US cents per gigabyte.

For real numbers, watch the Amazon console under Billing and Cost Management, and set a Budget with an email alert. That takes two minutes and it is the only figure that is actually yours.

> **Hrvatski.** Procjena koju alat ispiše odnosi se na **jednu kopiju**, pa je pomnoži s brojem dana zadržavanja uz pretpostavku jedne kopije dnevno. Svaka kopija je zaseban objekt i svaka se naplaćuje posebno, cijelo vrijeme dok je zaključavanje drži. Ako radiš backup tri puta dnevno, imaš tri puta više objekata i tri puta veći taj dio računa.
>
> **STANDARD_IA** je razred pohrane za podatke kojima se rijetko pristupa, jeftiniji za držanje a skuplji za čitanje, što backupu točno odgovara. Ima minimalno razdoblje naplate od 30 dana, pa objekt obrisan nakon 10 dana ipak plaćaš kao 30.
>
> **Egress** je promet prema van, dakle preuzimanje. Plaćaš ga samo kad stvarno vraćaš podatke, oko devet centi po gigabajtu.
>
> Za pravu sliku otvori u Amazonovoj konzoli **Billing and Cost Management** i postavi **Budget** s upozorenjem na mail. Procjena iz alata je gruba, račun je konačan.

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

> **Hrvatski.** Zašto je treća snimka najvažnija: sve ostalo pokazuje da alat radi, a treća pokazuje da zaštita radi. Server ima ispravne kredencijale, može dokazati da je spremnik zaključan, i **odbijen je** kad pokuša obrisati ili pročitati. To je točno ono što bi napadač isprobao.
>
> Četvrta snimka ide korak dalje. Tamo koristiš ključ s punim ovlastima, dakle nijedna politika ga ne sprječava, i Amazon ga svejedno odbija jer je preostao samo Object Lock. Administrator koji ne može obrisati backup je najuvjerljiviji dokaz koji postoji.
>
> Prije nego snimku staviš na GitHub, provjeri što je iznad naredbe u povijesti terminala. Ako se u kadru vidi redak s `export AWS_SECRET_ACCESS_KEY=`, taj ključ je javan i moraš ga odmah obrisati u konzoli.

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

> **Hrvatski.** **systemd timer** je zamjena za cron u modernom Linuxu. Datoteka `.service` opisuje što se pokreće, `.timer` kad se pokreće.
>
> **Exit code, izlazni kod.** Broj koji program vrati sustavu na kraju. Nula znači uspjeh, bilo što drugo grešku. systemd po tome zna je li posao prošao, pa se na to može zakvačiti obavijest.
>
> **`--max-age`.** Naredba `doctor` javlja grešku ako je zadnji backup stariji od te granice. Zadano je 26 sati, što odgovara dnevnom ritmu. Ako radiš backup svaka četiri sata, stavi `--max-age 5h` da bi propušteno pokretanje primijetio isti dan, a ne sutra.

## Restore

Restores use an administrator key, not the one on the database server. That key deliberately cannot read what it wrote.

```bash
lockbox restore --list
lockbox restore 2026-09-03T02-30-11Z
lockbox restore --to-file /tmp/dump.sql 2026-09-03T02-30-11Z   # inspect instead of loading
```

Do this on a spare machine before you trust any of it, and write down the elapsed time. That number is your recovery time. An untested backup is a rumour.

> **Hrvatski.** Vraćanje se **ne radi** ključem sa servera baze, jer taj ključ namjerno ne smije čitati. Za vraćanje koristiš admin ključ ili poseban ključ samo za čitanje, s bilo kojeg računala.
>
> `--to-file` skine i raspakira kopiju u običnu SQL datoteku umjesto da je odmah ubaci u bazu. Korisno kad želiš prvo pogledati što je unutra.

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

> **Hrvatski.** Ovo je najvažniji odjeljak u cijelom dokumentu.
>
> U spremniku leži **obična tekstualna datoteka sažeta gzipom**, doslovno ono što ispiše `mariadb-dump`. Nema vlastitog formata, nema indeksa, nema baze metapodataka, nema ključa koji drži samo lockbox. Zato ti za vraćanje **ne treba ni lockbox ni konfiguracija ni ključ sa servera**.
>
> Kod alata koji dedupliciraju to ne vrijedi. Tamo su podaci razlomljeni u komadiće koji nešto znače samo kroz taj alat i njegov indeks. Izgubiš indeks ili ne možeš pokrenuti alat, imaš gigabajte neupotrebljivih blokova.
>
> Ono što ti stvarno treba nakon potpunog gubitka: pristup Amazon računu, ime spremnika i regija, i tri naredbe gore. Ništa od toga nije bilo na serveru koji je pukao. Zapiši to na jednu stranicu i drži je izvan tvrtke, u upravitelju lozinki ili na papiru u sefu. **Ta stranica je plan oporavka, alat je samo pogodnost iznad njega.**

## Going to production

The quick start above is a lab exercise. Five things change for real use.

Put the bucket in **a separate Amazon account**. Otherwise an attacker who takes over the production account still cannot delete existing backups, thanks to COMPLIANCE mode, but can stop new ones from being written. Never let production credentials administer the backup account.

Use **COMPLIANCE mode with a real retention**. GOVERNANCE bends for anyone holding `s3:BypassGovernanceRetention`. Prove the flow on a small bucket with one day first, then create the production bucket with the retention you actually want. A retention already applied cannot be shortened.

Add **an alarm for the backup that does not happen**. A tool that fails silently is indistinguishable from one that was never installed. Hang an `OnFailure` hook on the systemd unit, or a CloudWatch alarm that fires when no new object has appeared inside your interval. A compromised server must not be able to publish those notifications itself, or it can drown you in noise.

Keep **a read only key off the server** so a restore does not need an administrator. Give it `s3:GetObject` and `s3:ListBucket` on the bucket and nothing else, and store it in a password manager rather than on any machine.

Run **a restore drill every three months** and write down the elapsed time. The only number management actually cares about is how long recovery takes.

> **Hrvatski.**
>
> **Zaseban Amazon račun.** Sada spremnik i produkcijski ključ žive u istom računu. COMPLIANCE ne dopušta napadaču brisanje postojećih kopija, ali može promijeniti pravila i zaustaviti nove. Odvojen račun, čije kredencijale produkcija nikad ne vidi, zatvara i tu rupu.
>
> **Uzbuna za backup koji se nije dogodio.** `OnFailure` je systemd postavka koja pokrene drugu jedinicu kad prva padne, na primjer slanje maila. CloudWatch je Amazonov nadzorni servis, u njemu se postavi alarm koji se javi ako se u spremniku dulje vrijeme nije pojavio nov objekt. Bitno: kompromitirani server ne smije sam moći slati te obavijesti, jer te inače može zatrpati lažnima dok radi štetu.
>
> **Vježba vraćanja svaka tri mjeseca.** Jedini broj koji upravu stvarno zanima je koliko traje oporavak. Bez vježbe taj broj ne znaš, samo pretpostavljaš.

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

> **Hrvatski.** **Socket authentication** znači da se program spaja na bazu preko lokalne datoteke utičnice, a ne preko mreže, i operacijski sustav jamči tko je pozivatelj. Zato na jednom serveru s rootom nema lozinke baze nigdje u konfiguraciji. Lozinka treba samo ako je baza na drugom stroju, i tada ide u zasebnu datoteku navedenu pod `defaults_file`.
>
> **Zamka koju treba znati:** izmjena ove datoteke **ne mijenja ništa u Amazonu**. Rok zaključavanja živi na spremniku, ne ovdje. Upišeš `days: 150` i dobit ćeš samo to da ti `doctor` javi neslaganje. Za drugi rok radiš nov spremnik novim `init`-om.

## Design notes

**Object Lock is not a one shot decision at bucket creation.** It used to be. Until November 2023 you had to contact AWS Support to add it to an existing bucket. Today `PutObjectLockConfiguration` turns it on for any versioned general purpose bucket, and the console offers it under Properties. lockbox still enables it at creation because that is the only moment the bucket is empty. Turning it on later does not retroactively lock what is already stored; for that you need S3 Batch Operations against an inventory report. Three related facts from the Amazon documentation before you commit: Object Lock requires versioning, once enabled it cannot be disabled and versioning cannot be suspended, and under compliance mode the only way to delete an object before its retention expires is to close the AWS account.

**Why the retention sits on the bucket, not on each object.** A bucket wide default retention applies to every upload automatically, so the uploading key never needs `s3:PutObjectRetention`. Given that permission it could also manipulate locks, which is exactly the hole this design closes and the one Kopia currently leaves open.

**Why verification uses ListObjectsV2 rather than HeadObject.** `HeadObject` is authorised as a read of the object and requires `s3:GetObject`, which the upload key does not have. Listing works, and comparing the stored size against the local size proves the object arrived intact.

**Why the dump is written to disk before uploading.** The size and both digests of the finished object have to be known before the request can be signed. Building the file first also means a dump that fails halfway never becomes a locked object that cannot be removed for a month.

**Why Content-MD5 goes on every upload.** A bucket with Object Lock enabled rejects `PutObject` without a checksum header. restic hit the same wall in [issue #2202](https://github.com/restic/restic/issues/2202) back in 2019. lockbox computes SHA-256 and MD5 in the same pass over the compressed stream, so the file is hashed once and sent once.

**Why `--single-transaction`.** It takes a consistent snapshot inside one transaction, so applications keep reading and writing while the dump runs. Stopping the database is never necessary for InnoDB. `doctor` warns when it finds MyISAM or Aria tables, because that guarantee does not cover them.

**Why no external libraries.** Signature Version 4 is a few hundred lines of standard hashing. Avoiding the vendor software development kit keeps the binary small, makes the program auditable in an afternoon, and means no dependency updates for a tool that should keep working untouched for years. The signing code is checked against Amazon's published example in `go test ./...`.

> **Hrvatski, pojam po pojam.**
>
> **Object Lock naknadno.** Do studenog 2023. Object Lock se mogao uključiti samo pri izradi spremnika, poslije je trebalo zvati Amazonovu podršku. Danas se uključuje i naknadno, na svakom spremniku s uključenim versioningom. lockbox ga svejedno uključuje odmah, jer je to jedini trenutak kad je spremnik prazan. Naknadno uključivanje **ne zaključava retroaktivno** ono što je već unutra, za to trebaju S3 Batch Operations nad inventurnim izvještajem. Tri stvari koje treba znati prije nego se odlučiš: traži versioning, ne može se isključiti, i u COMPLIANCE modu jedini način brisanja prije isteka je zatvoriti Amazon račun.
>
> **Rok na spremniku umjesto na objektu.** Rok se može postaviti svakom objektu posebno pri slanju, ali onda klijentu treba dozvola za postavljanje rokova, a to je upravo ona dozvola koju napadač želi. Zadani rok na razini spremnika Amazon primjenjuje sam, pa klijent tu dozvolu uopće ne treba.
>
> **HeadObject nasuprot ListObjectsV2.** `HeadObject` dohvaća podatke o jednom objektu, ali se u Amazonovim pravilima broji kao čitanje objekta i traži `s3:GetObject`. Naš ključ to nema. `ListObjectsV2` izlistava sadržaj spremnika s veličinama, pa je usporedba veličine dovoljna da se potvrdi da je datoteka stigla cijela.
>
> **Zašto dump ide prvo na disk.** Zahtjev prema Amazonu se potpisuje kriptografski, a u potpis ulazi i otisak sadržaja, koji se ne može znati dok se sadržaj ne dovrši. Uz to, dump koji pukne na pola nikad ne postane zaključan objekt kojeg se mjesec dana ne možeš riješiti.
>
> **Content-MD5.** Kontrolni zbroj sadržaja. Spremnik s Object Lockom odbija slanje bez njega, kao zaštitu od toga da se u trajno zaključan objekt spremi oštećena datoteka. Isti problem je restic imao 2019.
>
> **`--single-transaction`.** Dump se radi unutar jedne transakcije, pa vidi konzistentan snimak baze u jednom trenutku, a aplikacija cijelo vrijeme normalno radi. **Zaustavljanje baze nije potrebno.** Vrijedi za InnoDB, ne i za starije MyISAM i Aria tablice, pa `doctor` upozori ako ih nađe.
>
> **Signature Version 4.** Amazonov postupak potpisivanja zahtjeva. Svaki zahtjev se složi u točno propisan tekst i nad njim se izvede lanac kriptografskih sažimanja s tajnim ključem. lockbox to radi sam, u par stotina redaka, umjesto da povuče Amazonovu knjižnicu i s njom stotinjak datoteka tuđeg koda. Ispravnost je provjerena protiv Amazonovog objavljenog primjera s poznatim potpisom, unutar `go test ./...`.

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

> **Hrvatski.** **Multipart upload** je slanje velike datoteke u komadima. Amazon ga traži iznad 5 GB. lockbox ga nema, pa je to tvrda granica.
>
> **Logički dump** je tekstualni ispis naredbi koje ponovno izgrade bazu. Suprotnost je **fizički backup**, kopija samih datoteka baze, koji je za velike baze puno brži. Za to postoje `mariabackup` i Percona XtraBackup.
>
> **Recovery point objective.** Koliko podataka smiješ izgubiti, izraženo u vremenu. Ako radiš backup jednom dnevno, RPO je 24 sata. Skratiš li ga, radi backup češće.
>
> **Point in time recovery** je vraćanje na točnu sekundu, a ne na zadnju kopiju. Traži binarne logove baze i lockbox to ne pokriva.
>
> **Client side encryption** bi značilo da ti šifriraš podatke prije slanja, pa Amazon vidi samo nečitljive bajtove. Sada Amazon šifrira podatke, ali drži i ključeve. Za većinu poslovnih baza to je prihvatljivo, za osobne podatke pod strožim režimom nije.

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
