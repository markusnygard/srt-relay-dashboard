# SRT Relay Dashboard – käyttöohje

Suomenkielinen käyttöohje SRT Relay Dashboardille. Sovellus on kevyt SRT-välityspalvelin
(relay), jossa on selaimella käytettävä hallintapaneeli. Se välittää SRT-streamin yhdeltä
porttiparilta toiselle, joten sekä lähettäjä että katsoja liittyvät palvelimeen SRT-soittajina
(caller) – ei tarvita avoimia NAT-portteja eikä muita asiakaskonfiguraatioita.

## 1. Kirjautuminen

Avaa selaimella `http://PALVELIN:3001` ja kirjaudu sisään käyttäjänimellä ja salasanalla.
Oletustunnukset luodaan ensimmäisellä käynnistyksellä:

- **käyttäjänimi:** `admin`
- **salasana:** `admin`

Vaihda salasana heti ensimmäisen kirjautumisen jälkeen **Users**-painikkeesta (oikea yläkulma).

## 2. Streamin luominen

1. Valitse **Streams**-välilehti.
2. Kirjoita **Stream name** (esim. `camera1`).
3. Lisää halutessasi **Contact** (nimi/sähköposti/puhelin) – näkyy kaikkialla taulukossa,
   kalenterissa ja tiedoissa.
4. Valitse vapaa **ingress-portti** pudotusvalikosta (vain käyttämättömät portit näkyvät).
5. Paina **Add stream**.

Avautuvassa ikkunassa valitaan streamin kesto:

- **1 hour / 2 hours / 5 hours** – stream ajoitetaan heti alkavaksi ja automaattisesti
  päättyväksi (esim. 2 h päästä).
- **Manual** – avautuu ajastusikkuna, jossa aloitusajaksi on valmiiksi täytetty nykyhetki
  ja lopetusajaksi aloitus + 1 tunti (arvoja voi muokata).

Jokainen stream saa ingress-portin (lähettäjä) ja egress-portin (katsoja), oletuksena
+100. Hallintapaneeli näyttää kummallekin tarkan URL:n.

## 3. Lähettäjän yhdistäminen (OBS)

*Lähettäjä* työntää videon **sisään** välitykseen. OBS:ssä:

1. **Settings → Stream**.
2. **Service:** *Custom...*
3. **Server:** `srt://PALVELIN:IN_PORT?mode=caller&streamid=publish:STREAMID`
4. **Stream Key:** tyhjä.
5. **Start Streaming**.

Esimerkki: `srt://91.229.137.199:23001?mode=caller&streamid=publish:camera1`

> Hallintapaneeli näyttää valmiit URL:t jokaiselle streamille – kopioi vain "publish"-URL OBS:ään.

## 4. Katsojan yhdistäminen (OBS)

*Katsoja* hakee videon **ulos** välityksestä. Useita katsojia voi liittyä samaan streamiin
samanaikaisesti (fan-out). OBS:ssä lisää **Media Source**:

1. Lisää uusi **Media Source**.
2. **Input:** `srt://PALVELIN:OUT_PORT?mode=caller&streamid=read:STREAMID`
3. **Input Format:** `mpegts`.

Esimerkki: `srt://91.229.137.199:23101?mode=caller&streamid=read:camera1`

## 5. Stream-listan lukeminen

Jokaisella rivillä näkyy vasemmalta oikealle:

- **Kunto** (valo) – kokonaistila, katso alla.
- **Aika** – aloitus- ja lopetusaika (ilman päivämäärää). Lista on lajiteltu
  alkamisajan mukaan (päivämäärä vaikuttaa lajitteluun, vaikka sitä ei näytetä).
- **Stream** – nimi ja tila (`waiting`, `relaying`, `scheduled`).
- **Contact**, **StreamID** ja tarkat **publish**-URL.
- **Codecs** – streamista tunnistetut video-/äänikoodekit (esim. H.264, HEVC, AV1, AAC, Opus).
- **Format** – kuljetusformaatti: **MPEG-TS** tai **EFP**.
- **Bitrate** ja muut tilastot.

### Tilavalo

| Valo | Merkitys |
|------|----------|
| harmaa | lähettäjää ei vielä yhdistetty |
| keltainen | lähettäjä yhdistetty, ei katsojaa |
| vihreä | lähettäjä + vähintään yksi katsoja yhdistetty |
| punainen (vilkkuva) | lähettäjä yhdistetty, mutta yhteys on epävakaa |

## 6. Aikataulutus ja streamin muokkaus

- **Details** – koko streamin tiedot: portit, URL:t, contact, koodekit ja reaaliaikaiset
  tilastot.
- **Schedule** – aseta **aloitus**/**lopetus** -aika, **toistuvuus** (päivittäin/viikoittain)
  ja **auto-remove**. Tyhjä aloitusaika = alkaa heti, tyhjä lopetusaika = vain manuaalinen lopetus.
- **✕ (poista)** – poistaa streamin; porttipari vapautuu heti.

Jos **Auto-remove** on päällä, stream poistetaan automaattisesti lopetusajan jälkeen.

## 7. Kalenterinäkymä

**Calendar**-välilehti näyttää streamien aikataulut viikkonäkymänä:

- Vaihda **Day** / **Week** ylävasemman painikkeilla.
- Klikkaa vapaata aikaväliä luodaksesi ajoitetun streamin valmiiksi täytettynä.
- Klikkaa tapahtumaa muokataksesi sitä.
- Punainen **nyt-viiva** merkitsee nykyhetken.

## 8. Streamid-pohjainen reititys

Välitys tunnistaa lähettäjän **streamid:n** perusteella, ei portin mukaan:

- Lähettäjän `streamid=publish:STREAMID` täytyy vastata olemassa olevan streamin
  nimeä (tai `streamId`-kenttää).
- Streamiä ei tarvitse lähettää sen omalle ingress-portille – yhteys reitittyy
  oikean streamin egress-portille riippumatta siitä, mille portille se tulee.
- Jos streamid ei vastaa mitään streamia, lähettäjän yhteys **hylätään**.
- Jos streamid puuttuu kokonaan, yhteys ohjataan portin omalle streamille.

## 9. Käyttäjät

**Users**-painikkeesta voi lisätä käyttäjiä, vaihtaa salasanoja (🔑) ja poistaa tilejä.
Salasanat tallennetaan bcrypt-salasalahajotuksina, eikä niitä palauteta API:sta.

## Huomioita

- Välitys on **läpinäkyvä** – se ei uudelleenkoodaa, vaan kopioi paketit sellaisinaan.
  Koodekit siirtyvät muuttumattomina.
- MPEG-TS-streamista koodekit tunnistetaan PAT/PMT-taulukoista (H.264, HEVC, AV1, AAC, …).
  EFP-streamista koodekit luetaan suoraan paketin otsikosta.
- Katsojan ei tarvitse lähettää streamid:tä, mutta lähettäjän on hyvä antaa oikea.
- Aikataulut tallennetaan absoluuttisina hetkinä; selain näyttää ajat katsojan omassa
  aikavyöhykkeessä.

## Lisätietoa

Englanninkielinen, teknisempi dokumentaatio (käynnistys, liput, API, metrics) löytyy
tiedostosta [README.md](README.md).
