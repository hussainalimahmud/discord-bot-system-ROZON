# 🤖 Rozon Go Bot

Bot Discord berbasis Go untuk manajemen server komunitas,
moderasi, sistem poin, tiket bantuan, dan otomatisasi server.

![Language](https://img.shields.io/badge/Language-Go-00ADD8)
![Database](https://img.shields.io/badge/Database-SQLite-003B57)

Bot Discord ROZON berbasis Go (Golang) untuk manajemen server komunitas, dilengkapi dengan fitur-fitur berikut:

- **Slash Commands:** `/poin`, `/admin_poin`, `/admin_chat`, `/admin_role`, `/akses_bot`, `/bersihkan`, `/pengingat`, `/undian`, `/info_bot`, `/hapus_channel`.
- **Text Triggers:** `!uhuy`, `!login`, `g!aturan`, serta balasan sapaan otomatis dan salam pamit.
- **Panel Tiket Bantuan:** Pembuatan tiket privat dengan admin (bisa di-setup menggunakan `/admin_chat setup_tiket`).
- **Sistem Poin & Karma Role:** Poin bertambah saat chat dan berkurang jika terdeteksi berkata toxic. Role "Karma" member akan otomatis menyesuaikan poinnya. Tersedia juga fitur *toxic log* yang dilaporkan otomatis ke channel admin.
- **Sistem Keamanan Anti-Spam Soundboard:** Memberikan sanksi *timeout* 1 menit dan pemotongan 2 poin jika member melakukan spam soundboard. Admin bisa memanggil bot untuk mengawasi Voice Channel lewat `/admin_chat`.
- **Sistem Auto-Promotion:** Bot otomatis mendeteksi dan memberikan role kepada member lama (seperti Veteran / Senior) berdasarkan durasi mereka berada di dalam server.
- **Sistem Auto-Disconnect:** Bot "Satpam" otomatis meninggalkan Voice Channel jika tidak ada member manusia (hanya sendirian) selama 2,5 menit.

## Persyaratan

Sebelum menjalankan bot, pastikan kamu memiliki:

- [Go](https://go.dev/dl/) dengan versi yang sesuai dengan `go.mod`.
- Discord Bot Token dari Discord Developer Portal.
- Bot Discord yang sudah ditambahkan ke server dengan izin yang sesuai.

### Mendapatkan Discord Bot Token

1. Buka Discord Developer Portal.
2. Buat aplikasi baru atau pilih aplikasi bot yang sudah ada.
3. Buka bagian Bot.
4. Salin token bot dan simpan di file `.env`.
Contoh isi `.env`:

DISCORD_TOKEN=token_bot_kamu

**Penting:** Jangan pernah mengunggah file `.env` ke repository publik.
Jangan membagikan token bot kepada orang lain.

## Discord Intents dan Permissions

Pastikan bot memiliki intents dan permissions yang dibutuhkan oleh fitur yang digunakan.

Beberapa fitur mungkin membutuhkan:

- Server Members Intent.
- Message Content Intent.
- Guild Voice States Intent.
- View Channels.
- Send Messages.
- Manage Roles.
- Moderate Members.
- Manage Channels.

Kebutuhan sebenarnya bergantung pada implementasi kode dan konfigurasi server.
Berikan bot hanya izin yang diperlukan.

## Persiapan & Konfigurasi

1. **Pengaturan Token (.env):**
   Buat file bernama `.env` di dalam folder project, lalu isi dengan token bot Discord kamu dengan format seperti ini:
   `DISCORD_TOKEN=token_bot_kamu_di_sini`

2. **Pengaturan Data Server (config.go):**
   Buka file `config.go` dan sesuaikan nilainya dengan server kamu. Semua pengaturan wajib seperti ID Guild (Server), ID Channel, Nama Role, hingga daftar kata sapaan, pamitan, dan filter kata kasar semuanya terpusat di file ini.
   serta sesuaikan g!aturan dan role pada dc.go sesuai dengan keinginan.

## Jalankan lokal

Setelah konfigurasi di atas selesai, jalankan perintah berikut di terminal:

```bash
go mod tidy
go run .
```

## Catatan deploy Discloud

- Project ini sengaja pakai driver SQLite pure Go (`modernc.org/sqlite`) supaya lebih aman untuk environment deploy yang tidak nyaman dengan CGO. Semua data tersimpan otomatis di `database.db`.
- Pastikan file `logo.jpeg` ada di folder project kalau kamu ingin thumbnail pada command `/info_bot` tetap muncul.
- Saat melakukan update/commit kode ke Discloud, **jangan menimpa** file `database.db` dari lokal ke server agar data poin member tidak ter-reset. Gunakan fitur Backup di VS Code jika ingin mengamankan databasenya.

## Struktur Proyek

- `dc.go`: Source code utama bot.
- `config.go`: Konfigurasi server dan fitur bot.
- `go.mod`: Definisi module dan dependensi Go.
- `go.sum`: Checksum dependensi.
- `database.db`: Database SQLite untuk menyimpan data bot.
- `logo.jpeg`: Gambar thumbnail untuk informasi bot.
- `ROZON.png`: Aset gambar proyek.

```