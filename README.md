# HEPMjerenja

Aplikacija koja automatski preuzima podatke o potrošnji i proizvodnji električne
energije s [mjerenje.hep.hr](https://mjerenje.hep.hr), sprema ih u lokalnu SQLite
bazu i prikazuje kroz grafikone u web pregledniku.

Namijenjena je jednom HEP računu i pokretanju na vlastitom računalu ili
kućnom poslužitelju. Nema prijave, registracije ni korisničkih računa — pristupni
podaci za HEP čitaju se iz `config.ini` datoteke, a svi podaci ostaju kod vas.

> **Nije službena HEP aplikacija.** Projekt nije povezan s Hrvatskom
> elektroprivredom. Koristi isto neslužbeno API sučelje koje koristi i web
> stranica mjerenje.hep.hr, pa promjena na HEP-ovoj strani može privremeno
> onesposobiti preuzimanje podataka.

## Što aplikacija radi

- Automatski preuzima 15-minutna očitanja s HEP-ovog poslužitelja
- Pri prvom pokretanju preuzima cijelu povijest dostupnu za vaše mjerno mjesto
- Zatim svaki dan osvježava tekući mjesec kako bi uhvatila konačna očitanja
- **Mjesečni pregled** — krivulja u 15-minutnim intervalima, dnevni stupčasti
  grafikon, neto razlika (potrošnja − proizvodnja), prosjek po satu dana
- **Godišnji pregled** — mjesečni zbirni grafikoni, kalendarska toplinska karta,
  sažeci u widgetima
- **VT/NT razdvajanje** za bijeli i crveni tarifni model, s ispravnim ljetnim i
  zimskim granicama
- **Vremenski podaci** (sunčeva radijacija, temperature, izlazak/zalazak sunca) za
  lokaciju mjernog mjesta, preuzeti s Open-Meteo — korisno za usporedbu s
  proizvodnjom solarnih panela
- Ručno osvježavanje podataka gumbom **Osvježi podatke**

## Zahtjevi

- Račun na [mjerenje.hep.hr](https://mjerenje.hep.hr) (isti kojim se prijavljujete
  na HEP-ovu stranicu)
- **Docker** i Docker Compose — najlakši način, ili
- **Go 1.25+** ako želite pokretati iz izvornog koda

Baza podataka je jedna SQLite datoteka, pa nije potreban nikakav zaseban
poslužitelj baze.

## Instalacija

### Gotova binarna datoteka (najlakše)

Na stranici [Releases](https://github.com/edkirin/hepmjerenja/releases) preuzmite
arhivu za svoj sustav:

| Sustav | Datoteka |
|---|---|
| Linux (Intel/AMD) | `hepmjerenja_vX.Y.Z_linux_amd64.tar.gz` |
| Linux (ARM, npr. Raspberry Pi) | `hepmjerenja_vX.Y.Z_linux_arm64.tar.gz` |
| macOS (Apple Silicon) | `hepmjerenja_vX.Y.Z_darwin_arm64.tar.gz` |
| macOS (Intel) | `hepmjerenja_vX.Y.Z_darwin_amd64.tar.gz` |
| Windows | `hepmjerenja_vX.Y.Z_windows_amd64.zip` |

```bash
tar -xzf hepmjerenja_vX.Y.Z_linux_amd64.tar.gz
cp config.ini.example config.ini
# u config.ini upišite HEP_USERNAME i HEP_PASSWORD
./hepmjerenja
```

Aplikacija ne traži ništa dodatno — baza podataka, migracije, web stranice i baza
vremenskih zona ugrađene su u samu binarnu datoteku.

Datoteke nisu digitalno potpisane, pa ih sustav može zaustaviti: na macOS-u
pokrenite `xattr -d com.apple.quarantine hepmjerenja`, a na Windowsu u SmartScreen
dijalogu odaberite *More info → Run anyway*. Kontrolne sume su u `SHA256SUMS`.

Verziju provjerite s `hepmjerenja help`.

### Iz izvornog koda

```bash
git clone https://github.com/<vas-korisnik>/hepmjerenja.git
cd hepmjerenja

cp config.ini.example config.ini
# u config.ini upišite HEP_USERNAME i HEP_PASSWORD

make build            # ili: go build -o build/hepmjerenja ./app
./build/hepmjerenja
```

Aplikacija je dostupna na <http://localhost:8000>, a baza se stvara na putanji iz
`DB_PATH` (zadano `./data/hepmjerenja.db`).

Za razvoj s automatskim ponovnim pokretanjem (traži [air](https://github.com/air-verse/air)):

```bash
make run
```

### Docker

```bash
make docker-build     # gradi sliku hepmjerenja:latest
make docker-run       # pokreće kontejner na portu 8000
```

`make docker-run` montira vaš `config.ini` u kontejner (samo za čitanje) i Docker
volume `hepmjerenja-data` na `/app/data`, gdje se nalazi baza — bez toga bi baza
nestala pri brisanju kontejnera. Zadani `DB_PATH=./data/hepmjerenja.db` unutar
kontejnera pokazuje na `/app/data`, pa ista datoteka postavki radi i lokalno i u
Dockeru.

Isto ručno:

```bash
docker build -t hepmjerenja:latest .
docker run -d --name hepmjerenja \
    -v $(pwd)/config.ini:/app/config.ini:ro \
    -v hepmjerenja-data:/app/data \
    -p 8000:8000 \
    hepmjerenja:latest

# ili bez datoteke postavki, sve kroz varijable okoline:
docker run -d --name hepmjerenja \
    -e HEP_USERNAME=vas@email.hr -e HEP_PASSWORD=vasa-lozinka \
    -v hepmjerenja-data:/app/data \
    -p 8000:8000 \
    hepmjerenja:latest

docker logs -f hepmjerenja      # praćenje dnevnika
docker stop hepmjerenja         # zaustavljanje
```

Slika se gradi bez CGO-a, pa je binarna datoteka statična i kontejner radi kao
korisnik `33` (`www-data`). Direktorij `/app/data` u slici je unaprijed stvoren s
tim vlasnikom, tako da montirani volume naslijedi prava pisanja.

## Konfiguracija

Postavke se čitaju iz datoteke `config.ini` u korijenu projekta ili iz varijabli
okoline. Obavezni su samo HEP pristupni podaci.

Format je `KEY=vrijednost`, jedna postavka po retku — isto kao `.env` datoteka:

```ini
; komentari mogu počinjati s ; ili #
[hep]                       ; naslovi sekcija se zanemaruju
HEP_USERNAME=vas@email.hr
HEP_PASSWORD=vasa-lozinka
```

Sekcije su dopuštene samo kao vizualna podjela — nazivi postavki su ravni, pa
`[hep]` ne postaje dio imena. **Prave varijable okoline imaju prednost** nad
datotekom, što je korisno u Dockeru (`-e HEP_PASSWORD=…`). Ako `config.ini` ne
postoji, aplikacija radi samo s varijablama okoline, a za kompatibilnost se čita i
starija `.env` datoteka.

| Varijabla | Opis | Zadano |
|---|---|---|
| `HEP_USERNAME` | Korisničko ime za mjerenje.hep.hr | obavezno |
| `HEP_PASSWORD` | Lozinka za mjerenje.hep.hr | obavezno |
| `DB_PATH` | Datoteka SQLite baze | `./data/hepmjerenja.db` |
| `PORT` | Adresa na kojoj poslužitelj sluša | `:8000` |
| `LOG_DIR` | Direktorij za datoteke dnevnika | `.` |
| `LOG_LEVEL` | Razina zapisivanja (`debug`, `info`, `warn`, `error`) | `info` |
| `SQL_ECHO` | Ispis svih SQL upita na stdout (`true`/`false`) | `false` |
| `DEBUG` | Isključuje minifikaciju HTML/CSS/JS | `false` |

### Sigurnosno upozorenje

Aplikacija **nema prijavu ni autentikaciju** — svatko tko može doći do adrese i
porta vidi vaše podatke. HEP lozinka je u `config.ini` datoteci zapisana kao
nešifrirani tekst. Zato:

- ne izlažite aplikaciju izravno na internet,
- pokrećite je na lokalnoj mreži ili je stavite iza vlastitog reverse proxyja s
  autentikacijom,
- `config.ini` nikada ne dodajte u git (već je naveden u `.gitignore`).

## Korištenje

### Prvo pokretanje

Nakon pokretanja radnik u pozadini se prijavljuje na HEP, otkriva vaša mjerna
mjesta i preuzima svu dostupnu povijest očitanja. Za jedno mjerno mjesto s pola
godine podataka to traje oko pola minute. Napredak se vidi u dnevniku:

```
INF logged in                     user=vas@email.hr
INF collecting metering points    points=1
INF fetching month                month=2 year=2026
INF stored readings               inserted=2688 type=CONSUMPTION
```

Dok podataka nema, grafikoni su prazni — osvježite stranicu nakon što preuzimanje
završi.

### Pregledi

- **Mjesečno** (`/mjesecno`) — odaberite mjerno mjesto i mjesec. Krivulja se može
  zumirati i pomicati; klik na dan u dnevnom grafikonu prikazuje taj dan detaljno.
- **Godišnje** (`/godisnje`) — mjesečni zbirni prikaz za odabranu godinu i
  kalendarska toplinska karta cijele godine.
- **Postavke** (`/postavke`) — provjera HEP pristupnih podataka i odabir tarifnog
  modela po mjernom mjestu.

### Tarifni modeli

Odabir u Postavkama utječe na to prikazuje li se VT/NT razdvajanje:

| Model | Opis |
|---|---|
| **Plavi** | Jedinstvena dnevna tarifa — ista cijena kWh cijeli dan |
| **Bijeli** | Viša (VT) i niža (NT) tarifa. Zimi VT 07–21 h, ljeti VT 08–22 h. Zahtijeva višetarifno brojilo |
| **Crveni** | Kao bijeli, uz obračun vršne radne snage (€/kW). Za priključnu snagu veću od 22 kW |

Prijelaz na ljetno i zimsko računanje vremena obrađen je automatski, pa granica
VT/NT ljeti stvarno pada na 08:00 odnosno 22:00 po lokalnom vremenu.

### Ručno osvježavanje

Gumb **Osvježi podatke** u navigaciji odmah pokreće preuzimanje za mjesec koji
gledate i čeka rezultat. Korisno nakon što HEP objavi podatke za prethodni dan.
Ako preuzimanje ne uspije, poruka o grešci prikazuje se na mjesečnom pregledu.

### Naredbe iz terminala

```bash
hepmjerenja                       # pokreće web poslužitelj (uobičajeno)
hepmjerenja fetch [yyyy-mm]       # zakazuje ponovno preuzimanje mjeseca
hepmjerenja migrate [up|down|status]   # migracije baze
hepmjerenja help
```

U Dockeru: `docker exec hepmjerenja ./hepmjerenja migrate status`.

Migracije se primjenjuju automatski pri pokretanju, pa ih ručno pokrećete samo
kod održavanja. Dostupni su i `make migrate`, `make migrate-status` i
`make migrate-down`.

### Sigurnosna kopija

Cijela baza je jedna datoteka — kopirajte `data/hepmjerenja.db` (zajedno s
`-wal` i `-shm` datotekama ako postoje). Iz Dockera:

```bash
docker cp hepmjerenja:/app/data/hepmjerenja.db ./kopija.db
```

Aplikaciju je najbolje prije kopiranja zaustaviti, kako bi SQLite upisao sadržaj
`-wal` datoteke u glavnu bazu.

## Kako radi

### Preuzimanje podataka

Dvije gorutine rade paralelno:

- **Periodični radnik** — svake minute preuzima mjerna mjesta koja još nikad nisu
  preuzeta i ponovno preuzima ona starija od 6 sati. Jednom dnevno nakon 06:00
  osvježava tekući mjesec za sva mjerna mjesta.
- **Radnik za ručno preuzimanje** — obrađuje zahtjeve s gumba *Osvježi podatke*,
  neovisno o periodičnom radniku.

Nijedan ne kontaktira HEP kad nema posla.

Tok preuzimanja:

1. JWT token iz memorijskog cachea ili prijava na HEP API
2. Upis mjernih mjesta iz odgovora na prijavu; geokodiranje novih adresa
   (Photon / Nominatim) radi vremenskih podataka
3. Određivanje mjeseci za preuzimanje po mjernom mjestu
4. Preuzimanje potrošnje (smjer `P`) i/ili proizvodnje (smjer `R`)
5. Odbacivanje očitanja u budućnosti i uzastopnih nula na kraju niza
6. Upis s `ON CONFLICT DO NOTHING`, zatim osvježavanje dnevnih agregata za svaki
   mjesec kojeg upisana očitanja dotiču
7. Upis vremena zadnjeg uspješnog preuzimanja

HEP dopušta **jednu aktivnu sesiju po računu**: svaka nova prijava (i vaša
prijava u pregledniku) poništava prethodnu. Aplikacija to prepoznaje i automatski
se ponovno prijavljuje.

### Vremenska zona

Sva grupiranja po danima, mjesecima i VT/NT razdobljima računa SQLite pomoću
`localtime` modifikatora. Aplikacija sama postavlja `TZ` i `time.Local` na
`Europe/Zagreb` pri pokretanju, pa rezultati ne ovise o vremenskoj zoni
računala na kojem se pokreće.

## Tehnički detalji

### Tehnologije

- Go (paket `app/`) i [Echo](https://echo.labstack.com/) za HTTP
- [templ](https://templ.guide/) za HTML predloške
- SQLite preko [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) —
  implementacija u čistom Go jeziku, pa se Docker slika gradi bez CGO-a
- [goose](https://github.com/pressly/goose) za migracije (ugrađene u binarnu datoteku)
- [Chart.js 4](https://www.chartjs.org/) za grafikone
- zerolog za zapisivanje, godotenv za čitanje `config.ini`

Grafikoni se učitavaju s javnih CDN-ova (jsDelivr i cdnjs): Chart.js, plugin za
zumiranje, Hammer.js i SheetJS za izvoz u XLSX. Preuzimanje podataka s HEP-a i
prikaz brojeva rade bez interneta, ali za same grafikone preglednik mora doći do
CDN-a. Ako aplikaciju koristite u izoliranoj mreži, te datoteke treba posložiti
lokalno i zamijeniti adrese u `app/templates/charts/`.

### Struktura projekta

```
app/
  main.go         - Ulazna točka: Echo, rute, pokretanje radnika, TZ
  config.go       - Varijable okoline, DSN() i AppTimezone
  db.go           - SQLite veza + pomoćne metode za upite
  models.go       - Strukture podataka i modeli HEP API-ja
  const.go        - Raspored preuzimanja i ograničenja paralelnosti
  hep_client.go   - HTTP klijent za HEP API (prijava, očitanja)
  geocoder.go     - Adresa → koordinate (Photon / Nominatim)
  openmeteo.go    - Dnevni vremenski podaci i radijacija
  repository.go   - Svi SQL upiti
  worker.go       - Preuzimanje u pozadini, cache tokena, stanje zadnjeg preuzimanja
  handlers.go     - HTTP rukovatelji
  templates/      - templ predlošci (uređujte .templ, ne _templ.go)
    charts/       - Chart.js komponente
migrations/       - goose migracije (ugrađene, primjenjuju se pri pokretanju)
Dockerfile        - build u dvije faze; statična binarna datoteka bez CGO-a
Makefile          - build, migracije, Docker naredbe
```

### Baza podataka

Jedna SQLite datoteka, shema iz `migrations/`. Konvencije:

- **Vremenski trenuci** — `INTEGER`, unix epoch u sekundama, UTC
- **Datumi** — `TEXT` `'YYYY-MM-DD'` u lokalnom vremenu
- **Enumeracije** — `TEXT` s `CHECK` ograničenjem
- **Brojevi** — `REAL`

Tablice: `metering_points` (mjerna mjesta), `meter_readings` (15-minutna
očitanja, jedinstveno po `metering_point_code` + `timestamp` + `type`),
`daily_aggregates` (dnevni kWh), `daily_insolation` (vremenski podaci),
`skipped_metering_months` (mjeseci za koje HEP nema podatke).

### HTTP rute

| Ruta | Opis |
|---|---|
| `GET /` | preusmjerava na `/mjesecno` |
| `GET /mjesecno` | mjesečni pregled |
| `GET /godisnje` | godišnji pregled |
| `GET /postavke` | postavke |
| `POST /postavke/mjerno-mjesto/:code/tarifa` | promjena tarifnog modela |
| `GET /api/readings` | dnevna i 15-minutna očitanja za mjesec (JSON) |
| `GET /api/readings/year` | mjesečni zbirni podaci za godinu (JSON) |
| `GET /api/readings/hourly` | prosjek kWh po satu dana (JSON) |
| `GET /api/readings/calendar` | dnevni zbirni podaci za godinu (JSON) |
| `GET /api/insolation`, `GET /api/insolation/year` | vremenski podaci (JSON) |
| `POST /api/hep-test` | provjera HEP pristupnih podataka |
| `POST /api/fetch/now` | trenutno preuzimanje, čeka rezultat |

### Razvoj

```bash
make run                # air, automatsko ponovno pokretanje
templ generate          # obavezno nakon izmjene .templ datoteka
go build ./... && go vet ./...
```

### Izdavanje verzija

Spajanjem pull requesta u `main` pokreće se GitHub Actions workflow
(`.github/workflows/release.yml`) koji:

1. podigne *minor* verziju u odnosu na zadnju `vX.Y.Z` oznaku (bez oznaka počinje
   od `v0.1.0`),
2. prevede binarne datoteke za Linux, macOS i Windows (`CGO_ENABLED=0`, pa se svih
   pet kombinacija gradi na jednom Linux runneru),
3. objavi GitHub release s arhivama, kontrolnim sumama i popisom promjena.

Verzija se u binarnu datoteku upisuje kroz `-ldflags "-X main.version=…"`. Ako
promjenu ne želite izdati, u commit poruku dodajte `[skip release]`.

## Licenca

Copyright © 2026 Eden Kirin

[PolyForm Noncommercial License 1.0.0](LICENSE) — puni tekst je u datoteci
[`LICENSE`](LICENSE).

Ukratko, i bez pravne težine:

- **Slobodno korištenje u nekomercijalne svrhe** — osobna upotreba, kućna
  instalacija, obrazovanje, istraživanje, neprofitne organizacije i javne
  ustanove.
- **Izmjene su dopuštene** — možete mijenjati kod, stvarati izvedene radove i
  dijeliti ih dalje, uz zadržavanje ove licence i obavijesti o autorstvu.
- **Komercijalna upotreba nije dopuštena** bez zasebnog dogovora s autorom.

Napomena: zbog ograničenja komercijalne upotrebe ovo formalno nije
„open source“ licenca po definiciji [OSI-ja](https://opensource.org/osd) —
izvorni kod je javno dostupan, ali uvjeti su restriktivniji od npr. MIT ili
Apache licence. GitHub će je prikazati kao nestandardnu licencu.

## Doprinosi

Prijave grešaka i prijedlozi dobrodošli su kroz GitHub Issues. Za promjene koda
otvorite pull request s kratkim opisom što i zašto mijenjate.
