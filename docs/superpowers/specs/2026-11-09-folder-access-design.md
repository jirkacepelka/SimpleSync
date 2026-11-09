# Přístup členů k jednotlivým složkám vaultu

> **Nahrazeno ve verzi 0.9.0.** Model pravidel s „Bez přístupu“, základem role
> a „nejkonkrétnější vyhrává“ byl pro uživatele nepřehledný. Od 0.9.0 platí:
> po zapnutí člen (kromě vlastníků) nevidí nic; role je seznam složek
> (Čtení / Úpravy); člen může mít víc rolí a k tomu složky navíc; vidí součet
> všeho a Úpravy mají přednost před Čtením. Vynucování (`store.Scoped`) zůstalo.
> Data z 0.8.0 převádí `migrateFolderAccess` v `server/internal/store/access.go`.

## Kontext

SimpleSync dnes řídí přístup jen na úrovni celého vaultu: člen má jednu roli
(Jen čtení / Úpravy / Vlastník) a ta platí pro všechny soubory. Chcete mít
možnost nastavit, kdo vidí a kdo smí upravovat které složky (např. uvnitř
„Program 2029"). Dohodnuté zadání:

- u složky lze nastavit **bez přístupu / jen čtení / úpravy**;
- pravidla jdou nastavit na **vlastních pojmenovaných rolích** i jako
  **výjimky u jednotlivého člena**;
- funkce je **volitelná, zapíná se v nastavení konkrétního vaultu ve webové
  administraci serveru** (Vaulty → vault → Nastavení; ne v Obsidianu) a ve
  výchozím stavu je vypnutá, takže kdo ji nepotřebuje, nic nového neuvidí.

Role se dnes kontroluje na třech vstupech (`api.go:114`, `web.go:395`,
`mcp/tools.go:195`) a za nimi už žádný kód cestu souboru neřeší. Skrytý obsah
proto může uniknout mnoha cestami (seznam změn, historie, koš, ZIP, zálohy,
hledání přes MCP). Návrh stojí na jednom centrálním místě, přes které musí
projít každé čtení i zápis.

## Jak se to bude chovat

### Zapnutí

- Ve webové administraci serveru, na stránce Nastavení daného vaultu, přibude
  přepínač **„Práva po složkách"** (jen vlastník vaultu nebo admin). Platí
  jen pro tento vault; ostatní vaulty zůstanou beze změny.
- **Vypnuto (výchozí):** vše funguje přesně jako dnes. Záložka Role ani
  nastavení výjimek se nezobrazují a žádná pravidla se nevyhodnocují.
- Při vypnutí zůstanou role a pravidla uložené, ale přestanou platit: všichni
  členové znovu uvidí celý vault podle své základní role. Přepínač na to před
  vypnutím upozorní.

### Role a pravidla

- **Vestavěné role** (Jen čtení / Úpravy / Vlastník) zůstávají beze změny.
- **Vlastní role** si vlastník vytvoří pro daný vault (např. „Finance").
  Role má název, základ (Jen čtení nebo Úpravy) a pravidla
  „složka → bez přístupu / jen čtení / úpravy".
- Člen má právě jednu roli: vestavěnou, nebo vlastní. Ve výběru role u člena
  se vlastní role objeví vedle tří vestavěných.
- **Výjimky člena:** jednotlivému členovi lze přidat vlastní pravidla pro
  složky, ať má roli vestavěnou, nebo vlastní.
- **Vyhodnocení:** platí pravidlo s nejkonkrétnější (nejdelší) cestou;
  pravidlo pro složku platí i pro vše uvnitř. Při stejné cestě má výjimka
  člena přednost před pravidlem role. Kde žádné pravidlo neplatí, rozhoduje
  základ role.
- Volba **„zbytek vaultu skrýt"** (pravidlo na kořen) umožní režim „vidí jen
  vyjmenované složky".
- Pravidlo nikdy nedá víc než základ role: kdo má základ Jen čtení, nemůže
  nikde psát.
- Vlastníci vaultu a administrátoři mají vždy plný přístup.
- Cesty se porovnávají bez ohledu na velikost písmen (`pathutil.Fold`), aby
  nešlo pravidlo obejít složkou `finance` vedle `Finance`.

### Co uvidí omezený člen

- Skrytý soubor pro něj neexistuje: není v Obsidianu, ve webovém editoru,
  v historii, koši, ZIPu, hledání ani v odkazech.
- **Zálohy:** člen, pro kterého platí jakékoli pravidlo, záložku Zálohy
  nevidí (záloha je snímek celého vaultu včetně hotových ZIPů).
- **Odebrání přístupu:** plugin přesune už stažené soubory z dané složky do
  koše Obsidianu. Soubory, které člen mezitím lokálně upravil, ponechá a jen
  je přestane synchronizovat.
- **Složka jen pro čtení:** lokální úpravy se neodešlou a plugin to oznámí
  (stejně jako dnes u role Jen čtení pro celý vault).

### Omezení, o kterých je dobré vědět

- Pravidlo je vázané na cestu. Přejmenování složky ve webovém editoru pravidla
  přesune. Přejmenování v Obsidianu server nepozná (přijde jako smazání
  a vytvoření), takže pravidlo zůstane na staré cestě. Pro citlivý obsah je
  proto bezpečnější režim „zbytek skrýt + povolit vybrané složky"; záložka
  Role upozorní na pravidla mířící na neexistující složku.
- Server pravidla vynucuje vždy. Starší verze pluginu ale budou u odmítnutých
  zápisů hlásit chybu a nově zpřístupněné složky nestáhnou, dokud se plugin
  neaktualizuje.
- Počet souborů vaultu na přehledu zůstane celkový.

## Návrh řešení

### 1. Úložiště a centrální kontrola (`server/internal/store/`)

- Schéma v `store.go` (tabulky přes `CREATE TABLE IF NOT EXISTS`, sloupce přes
  `ALTER TABLE` stejně jako `devices.vault_id` na `store.go:144`):
  - `vaults.folder_access` (0/1, výchozí 0) – přepínač;
  - `vaults.access_rev` – zvýší se při každé změně přepínače, rolí, pravidel
    nebo členství;
  - `vault_roles(id, vault_id, name, base)` – vlastní role, název jedinečný
    ve vaultu;
  - `vault_members.role_id` – odkaz na vlastní roli (`NULL` = vestavěná);
  - `vault_access_rules(id, vault_id, role_id, user_id, path, path_fold, level)`
    – pravidlo patří buď roli, nebo členovi; vše s `ON DELETE CASCADE`.
- `Store.Role` u člena s vlastní rolí vrací její základ, takže všechny dnešní
  kontroly `RoleAtLeast` fungují beze změny.
- Nový soubor `store/access.go`:
  - `type Access struct` (základ, pravidla role, výjimky člena, `Rev`)
    s metodami `CanRead(path)`, `CanWrite(path)`, `Restricted()`.
    Při vypnutém přepínači, u vlastníka a u admina je bez pravidel.
  - `Store.Access(ctx, user, vaultID)` nahradí volání `Store.Role` ve třech
    obalech.
  - správa: `CreateVaultRole`, `UpdateVaultRole`, `DeleteVaultRole`,
    `VaultRoles`, `SetAccessRule`, `RemoveAccessRule`, `AccessRules`,
    `MoveAccessRules(old, new)`, `SetFolderAccess`. Smazání role vrátí její
    členy na vestavěnou roli podle základu; `RemoveMember` smaže i výjimky.
  - `type Scoped struct` (vault + Access) se stejnými metodami jako dnes
    `Store`, ale filtrovanými: `ListFiles`, `File`, `Versions`, `Version`,
    `RecentVersions`, `Changes`, `Commit`, `HashReadable`.
- `Scoped.Commit` odmítne operace mimo zapisovatelné cesty novým kódem
  `forbidden` (platí i pro vynucené operace při obnově verze).
- `Scoped.Changes` filtruje podle `CanRead` a načítá další stránky, dokud
  nenajde viditelnou položku. Klient totiž při prázdné stránce s `more`
  přeskočí na `head` (`engine.ts:160`) a o změny by přišel.
- `Scoped.HashReadable` u omezeného člena ověří, že na hash odkazuje aspoň
  jedna čitelná cesta (dnes `HashInVault` stačí kdekoli ve vaultu).

### 2. Vstupní body

Všechny obaly uloží `Scoped` do kontextu požadavku a handlery přestanou volat
souborové metody přímo na `Store`.

- **Sync API** (`api/api.go`): `changes`, `getBlob`, `missingBlobs`, `commit`.
  Odpověď `changes` nově nese `access` (verze + výsledná pravidla volajícího).
  Po změně pravidel se zavolá `Hub.Notify`, aby se zařízení probudila.
- **Web** (`web/handlers_editor.go`, `handlers_vault.go`,
  `handlers_obsidian.go`, `handlers_backup.go`): strom, poznámka, render
  odkazů, uložení, přejmenování, smazání, nahrání, raw, historie, obnovení
  verze, koš, oba ZIPy. Trasy záloh vrátí omezenému členovi 403 a záložka se
  skryje (`templates/partials.html`). `apiNote` vrací příznak zapisovatelnosti
  konkrétní poznámky a `static/notes.js` podle něj přepne editor do čtení.
  `apiRename` složky volá `MoveAccessRules`.
- **MCP** (`mcp/tools.go`): `vault()` vrací i `Scoped`; `live`, `find`,
  `commit`, `listNotes`, `searchNotes`, `recentChanges` a zápisové nástroje
  jdou přes něj.

### 3. Webová administrace

- **Nastavení vaultu** (`templates/vault_settings.html`,
  `vaultSettingsSave`): přepínač „Práva po složkách" s upozorněním při
  vypnutí.
- Vše další se zobrazí jen při zapnutém přepínači:
  - **Nová záložka „Role"** (`templates/vault_roles.html`, trasy
    `/vaults/{id}/roles…`, jen vlastník): seznam vlastních rolí, vytvoření,
    přejmenování, základ, smazání a u každé role pravidla pro složky
    (složka z nabídky existujících přes `<datalist>`, úroveň, odebrat).
  - **Záložka Členové** (`templates/vault_members.html`): výběr role nabídne
    i vlastní role; u člena kromě vlastníků rozbalovací blok „Výjimky" se
    stejným formulářem pravidel.
- Nové handlery v novém souboru `web/handlers_access.go`, trasy ve `web.go`.
- Texty do všech 8 jazyků v `server/internal/i18n/locales/*.json`.

### 4. Plugin (`plugin/src/engine/`)

- `client.ts`, `types.ts`: typ `access` v odpovědi `changes`; `accessVersion`
  ve stavu synchronizace.
- `engine.ts`:
  - při změně `accessVersion` stáhnout změny znovu od revize 0 (nově
    zpřístupněné soubory; nezměněné se přeskočí porovnáním hashů);
  - položky v `base`, které už nejsou čitelné: nezměněný soubor do koše
    (`fs.trash`), upravený ponechat, obojí odebrat z `base`;
  - při odesílání přeskočit cesty bez práva zápisu a jejich počet oznámit;
  - kód `forbidden` z commitu převést na srozumitelnou hlášku.
- Plugin žádné vlastní nastavení nedostane; řídí se tím, co pošle server.
- Texty do 8 jazyků v `plugin/src/i18n/*.ts`.

### 5. Dokumentace

- Sekce v `README.md`, záznam v `CHANGELOG.md`.
- Tento návrh uložit jako
  `docs/superpowers/specs/2026-11-09-folder-access-design.md`.

## Postup

Po krocích s testy psanými předem, každý krok samostatný commit na větvi
`claude/cool-allen-4xhem1`:

1. Úložiště: schéma, přepínač, vlastní role, `Access`, `Scoped` + testy.
2. Sync API.
3. Web (čtení, zápis, zálohy).
4. MCP.
5. Administrace: přepínač, záložka Role, výjimky u členů + překlady.
6. Plugin + překlady.
7. README a CHANGELOG.

Číslo verze nezvyšuji a pull request nevytvářím, dokud o to neřeknete.

## Ověření

- `cd server && go test ./...`
  - `store/store_test.go`: vyhodnocení pravidel (nejdelší shoda, výjimka
    člena proti roli, velikost písmen, strop základu, kořenové pravidlo,
    vypnutý přepínač = žádné omezení), smazání role, stránkování filtrovaných
    změn.
  - nový `app/access_test.go` podle vzoru `app/editor_test.go`: pro omezeného
    člena projde každý vstupní bod (changes, blob podle hashe, commit, strom,
    poznámka, raw, historie, koš, ZIP, zálohy, MCP list/search/read/write)
    a ověří, že skrytá cesta ani její obsah se nikde neobjeví a zápis mimo
    povolené složky je odmítnut. Stejné kontroly pro vlastníka a pro vault
    s vypnutým přepínačem musí projít beze změny.
- `cd plugin && npm test`: `test/sync.test.ts` a `test/server.ts` rozšířit
  o přidělení přístupu (soubory se stáhnou), odebrání (do koše, upravené
  zůstanou) a složku jen pro čtení (nic se neodešle).
- Ručně v prohlížeči: spustit server, vytvořit vault se složkou
  „Program 2029" a dvěma podsložkami; ověřit, že bez zapnutí se v administraci
  nic nezměnilo; zapnout, vytvořit roli, druhému uživateli jednu podsložku
  skrýt a druhou dát jen pro čtení; zkontrolovat webový editor, záložky Role
  a Členové; vypnout a ověřit návrat k původnímu chování.
