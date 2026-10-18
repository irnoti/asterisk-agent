# پروتکل Agent ↔ API

Agent فقط **اتصال خروجی HTTPS** به API می‌زند؛ پورتی روی PBX باز نمی‌شود. API درخواست‌های ARI را در صف می‌گذارد، Agent با long-poll می‌گیرد، روی ARI محلی اجرا می‌کند و جواب را برمی‌گرداند. رویدادهای ARI هم برعکس به API می‌روند. همهٔ مسیرها زیر `/api/v1/agent` هستند.

## احراز هویت
`Authorization: Bearer irna_<token>` روی همهٔ درخواست‌ها. توکن را API یک‌بار نمایش می‌دهد و فقط hash آن را نگه می‌دارد؛ قابل ابطال است. `401` یعنی توکن ابطال یا اشتباه است (Agent منتظر می‌ماند و دوباره امتحان می‌کند). رمز ARI روی خود PBX ساخته می‌شود و **هرگز به API فرستاده نمی‌شود**: Agent خودش هنگام اجرای هر درخواست آن را اضافه می‌کند.

## مسیرها

| مسیر | بدنه | پاسخ |
|---|---|---|
| `POST /hello` | `{version, hostname, os, arch, asterisk}` | `{agentId, app, pollWaitSec, configVersion}` |
| `GET /config` | — | `{version, files:[{name, content, sha256}]}` |
| `POST /poll` | `{}` | `{requests:[{id, method, path, body?}]}` (بعد از حداکثر `pollWaitSec` ثانیه خالی) |
| `POST /respond` | `{id, status, body, encoding, error?}` | `204` |
| `POST /events` | `{events:[<رویداد ARI>]}` تا ۵۰ تا | `204` |
| `POST /heartbeat` | `{channels, ariUp, uptimeSec}` هر ۳۰ ثانیه | `{configVersion}` |

- `path` در `/poll` مسیر ARI بدون پیشوند `/ari` است، با query (مثلاً `/channels?endpoint=PJSIP/1001&app=irnoti`).
- `encoding`: `utf8`، یا `base64` برای محتوای باینری (فایل ضبط‌شده، `GET /recordings/stored/{name}/file`).
- اگر `configVersion` در heartbeat با نسخهٔ اعمال‌شده فرق کند، Agent `GET /config` می‌زند و فقط فایل‌های تغییرکرده را می‌نویسد و `dialplan reload` می‌کند.

## چه چیزی از تونل رد می‌شود
فقط درخواست‌هایی که خود پلتفرم لازم دارد (فهرست سفید در `internal/proxy/allowlist.go`): کانال‌ها (originate، answer، play، record، snoop، continue، redirect، hold، ring، dtmf، قطع)، bridge‌ها، توقف playback، توقف و دریافت و حذف ضبط، و ساخت/حذف `auth|aor|endpoint` برای PJSIP. هر چیز دیگر (اشتراک رویداد، applications، ماژول‌ها، logging، تنظیمات غیر PJSIP …) و هر مسیر با `..`، `//`، `%2f`، `%2e` یا `api_key` در query رد می‌شود (`403`).

## تنظیمات Asterisk که API می‌فرستد
Bundle فقط **فایل‌های `.conf` کوچک** است و Agent هر کدام را قبل از نوشتن بررسی می‌کند:
- نام فایل `^[a-z0-9][a-z0-9_.-]{0,63}\.conf$`، حداکثر ۱۰ فایل و ۱۲۸ کیلوبایت هر کدام؛ `sha256` باید با محتوا بخواند.
- فایل `ari_irnoti.conf` را فقط خود Agent می‌نویسد؛ API نمی‌تواند کاربر ARI تعریف کند.
- فقط **برنامه‌های مجاز دایل‌پلن**: `NoOp Set ExecIf GotoIf Goto Playback MixMonitor Dial Hangup Stasis Answer Wait Ringing Progress Congestion Busy Return`. توابع `SHELL CURL ODBC_* FILE ENV AGENT_* DB REALTIME* …` و دستورهای `#include`/`#exec` رد می‌شوند. پس API آسیب‌دیده هم نمی‌تواند روی PBX دستور اجرا کند.
- فایل‌ها فقط داخل `<asterisk-dir>/irnoti/` نوشته می‌شوند. تنها فایل‌هایی که Agent در جاهای دیگر دست می‌زند: بلوک مدیریت‌شده در `ari.conf`، `http.conf` (فقط اگر وب‌سرور از قبل روشن نباشد) و فایل دایل‌پلن (`extensions_custom.conf` یا `extensions.conf`) با نشانگرهای `; BEGIN/END irnoti-asterisk`.
- پیش از هر تغییر از فایل‌های موجود پشتیبان گرفته می‌شود (`/var/lib/irnoti-asterisk/backups/<زمان>/`). اگر `reload` یا آزمون ARI شکست بخورد، همه به حالت قبل برمی‌گردد (`rollback` هم دستی در دسترس است).

## رفتار در قطع اتصال
رویدادهای ARI تا ۵٬۰۰۰ تا در صف می‌مانند (در پرشدن، قدیمی‌ترها دور ریخته می‌شوند)؛ ارسال تا ۸ بار با backoff تکرار می‌شود. poll با backoff (۱ تا ۳۰ ثانیه) ادامه پیدا می‌کند. تماس‌هایی که وسط قطع داخل Stasis بودند توسط Asterisk قطع می‌شوند؛ تماس‌های جدید با `Goto` در دایل‌پلن به مسیر قبلی برمی‌گردند.
