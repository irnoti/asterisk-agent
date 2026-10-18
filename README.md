# irnoti-asterisk

برنامهٔ کوچکی که روی **سرور Asterisk** مشتری نصب می‌شود و دو کار می‌کند:

1. **نصب و تنظیم خودکار:** `http.conf`، `ari.conf` و دایل‌پلن IRNoti را در فایل‌های جدا و بلوک‌های مدیریت‌شده می‌نویسد، با پشتیبان‌گیری، آزمون و بازگردانی خودکار.
2. **تونل به ARI:** با اتصال **خروجی** HTTPS به API وصل می‌ماند و درخواست‌های ARI پلتفرم را روی PBX محلی اجرا می‌کند. هیچ پورتی باز نمی‌شود.

هیچ منطقی در Agent نیست: تعرفه، سقف مکالمه، مسیردهی و صورتحساب همه در API می‌مانند. جزئیات در [docs/PROTOCOL.md](docs/PROTOCOL.md).

## نصب
در پنل ادمین (`/admin/voip`) یک Agent بسازید و توکن را بردارید. روی سرور Asterisk (به‌عنوان root):

```bash
curl -fsSL https://raw.githubusercontent.com/irnoti/asterisk-agent/main/scripts/install.sh | sudo bash -s -- \
  --api https://api.example.com --token irna_xxxxxxxx
```
اسکریپت فایل اجرایی مناسب CPU را از Releases می‌گیرد، با `SHA256SUMS` می‌سنجد، در `/usr/local/bin/irnoti-asterisk` می‌گذارد و `irnoti-asterisk install` را اجرا می‌کند. قبل از هر تغییر می‌توانید `--dry-run` بزنید.

بعد از نصب، ورودی ترانک را به کانتکست `irnoti-inbound` وصل کنید (در FreePBX: مقصد سفارشی `irnoti-inbound,${EXTEN},1`؛ در `pjsip.conf` معمولی: `context=irnoti-inbound` روی endpoint ترانک).

## دستورها
```
irnoti-asterisk install   --api URL --token TOKEN [--asterisk-dir DIR] [--extensions-file FILE] [--no-service] [--dry-run]
irnoti-asterisk run       [--config FILE]     # دیمون؛ با systemd اجرا می‌شود
irnoti-asterisk status
irnoti-asterisk rollback                      # برگرداندن فایل‌ها به پشتیبان آخرین تغییر
irnoti-asterisk uninstall [--purge]           # حذف بلوک‌ها و پوشهٔ irnoti/؛ --purge تنظیمات Agent را هم پاک می‌کند
```
سرویس: `systemctl status irnoti-asterisk`، لاگ: `journalctl -u irnoti-asterisk -f`.

## امنیت
- توکن Agent فقط روی سرور خودش در `/etc/irnoti-asterisk/agent.json` (دسترسی ۰۶۰۰) و در API فقط به‌صورت hash است. ابطال از پنل.
- رمز ARI روی همان PBX ساخته می‌شود و به API نمی‌رسد.
- فقط درخواست‌های فهرست سفید به ARI می‌رسند، و فقط دایل‌پلنِ بدون `System/AGI/SHELL/…` نوشته می‌شود؛ بنابراین حتی API آسیب‌دیده نمی‌تواند روی PBX دستور اجرا کند.
- کد را مشتری می‌بیند و امنیت به محرمانه‌بودن کد تکیه ندارد: توکن‌ها جدا و قابل ابطال‌اند و هر تصمیم مالی در API است.

## توسعه
```bash
go vet ./... && go test ./... -race
make build        # فایل‌های ایستا برای linux/amd64 و linux/arm64 در dist/
```
Go 1.24+. وابستگی بیرونی فقط `github.com/coder/websocket` (برای رویدادهای ARI).
