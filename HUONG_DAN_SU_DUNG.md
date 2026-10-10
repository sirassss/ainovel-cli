# Hướng Dẫn Sử Dụng ainovel-cli (Tiếng Việt)

`ainovel-cli` là công cụ dòng lệnh (CLI) sáng tác tiểu thuyết dài kỳ tự động bằng AI với kiến trúc **Đa Agent (Coordinator → Architect → Writer → Editor)**. Giao diện người dùng (TUI) được Việt hóa 100% thân thiện và hỗ trợ sáng tác linh hoạt bằng cả **Tiếng Việt (mặc định)** hoặc **Tiếng Trung**.

---

## 📑 Mục Lục
1. [Yêu Cầu Hệ Thống](#1-yêu-cầu-hệ-thống)
2. [Cài Đặt Nhanh](#2-cài-đặt-nhanh)
3. [Tùy Chọn Ngôn Ngữ Sáng Tác (Tiếng Việt / Tiếng Trung)](#3-tùy-chọn-ngôn-ngữ-sáng-tác-tiếng-việt--tiếng-trung)
4. [Cấu Hình Nhà Cung Cấp AI (LLM)](#4-cấu-hình-nhà-cung-cấp-ai-llm)
   - [Cách 1: Dùng Ollama Cục Bộ (Miễn phí, 100% Offline)](#cách-1-dùng-ollama-cục-bộ-miễn-phí-100-offline)
   - [Cách 2: Dùng Cloud API (OpenRouter, Gemini, Claude, OpenAI, DeepSeek)](#cách-2-dùng-cloud-api-openrouter-gemini-claude-openai-deepseek)
   - [Cách 3: Phối hợp nhiều Model theo vai trò (Tối ưu chi phí & chất lượng)](#cách-3-phối-hợp-nhiều-model-theo-vai-trò-tối-ưu-chi-phí--chất-lượng)
5. [Bắt Đầu Sáng Tác](#5-bắt-đầu-sáng-tác)
   - [Giao diện TUI trực quan](#giao-diện-tui-trực-quan)
   - [Chế độ Headless (Chạy ngầm server)](#chế-độ-headless-chạy-ngầm-server)
6. [Quản Lý Sáng Tác Nhiều Truyện Độc Lập](#6-quản-lý-sáng-tác-nhiều-truyện-độc-lập)
7. [Bảng Lệnh Điều Khiển Trong TUI (Slash Commands)](#7-bảng-lệnh-điều-khiển-trong-tui-slash-commands)
8. [Xuất Truyện (TXT / EPUB)](#8-xuất-truyện-txt--epub)
9. [Quy Tắc & Lưu Ý Quan Trọng](#9-quy-tắc--lưu-ý-quan-trọng)

---

## 1. Yêu Cầu Hệ Thống

- **Khuyến nghị nhất**: Đã cài đặt [Docker](https://www.docker.com/) & Docker Desktop (Windows / macOS / Linux).
- **Hoặc Chạy từ Source**: Máy đã cài đặt [Go](https://go.dev/) ≥ 1.21.
- **LLM**: 
  - Máy có GPU (Nvidia VRAM ≥ 12GB) nếu muốn chạy Ollama cục bộ với model Qwen 2.5 / 3.5.
  - Hoặc API key từ OpenRouter, Anthropic, Google Gemini, OpenAI, DeepSeek.

---

## 2. Cài Đặt Nhanh

### Bước 1: Clone Repo & Chuẩn Bị Thư Mục
```bash
git clone https://github.com/AnhDT955/Ainovel-cli.git
cd Ainovel-cli

# Tạo thư mục chứa cấu hình và thư mục chứa truyện
mkdir -p config workspace novels
```

### Bước 2: Build Docker Image
```bash
docker compose build
```
*(Nếu build từ source Go: `go build -o ainovel-cli ./cmd/ainovel-cli`)*

---

## 3. Tùy Chọn Ngôn Ngữ Sáng Tác (Tiếng Việt / Tiếng Trung)

Trong file cấu hình `config/config.json`, bạn có thể chỉ định trường `"language"`:
- `"language": "vi"` (Mặc định): Toàn bộ dàn ý, nhân vật, bối cảnh thế giới, quy chuẩn văn phong chống sáo rỗng AI và nội dung từng chương sẽ được sinh ra bằng **Tiếng Việt** tự nhiên, mượt mà.
- `"language": "zh"`: Nội dung truyện được sinh ra bằng **Tiếng Trung** nguyên bản (phù hợp nếu bạn viết truyện Trung hoặc muốn dịch sau).

> 💡 **Ghi chú**: Giao diện hiển thị, bảng điều khiển TUI, trạng thái và các thông báo lỗi luôn hiển thị **100% bằng Tiếng Việt**.

---

## 4. Cấu Hình Nhà Cung Cấp AI (LLM)

Tạo file `config/config.json` trong thư mục `Ainovel-cli`:

### Cách 1: Dùng Ollama Cục Bộ (Miễn phí, 100% Offline)

1. **Tạo model trên Ollama với Context window 65536 tokens**:
   - Mở PowerShell / Terminal và chạy:
     ```powershell
     @"
     FROM qwen2.5:14b
     PARAMETER num_ctx 65536
     "@ | Out-File -FilePath "$env:TEMP\ainovel.Modelfile" -Encoding ascii

     ollama create ainovel-qwen -f "$env:TEMP\ainovel.Modelfile"
     ```
2. **Nội dung `config/config.json`**:
   ```json
   {
     "language": "vi",
     "provider": "ollama",
     "model": "ainovel-qwen",
     "providers": {
       "ollama": {
         "base_url": "http://host.docker.internal:11434/v1",
         "stream_idle_timeout": "300s"
       }
     },
     "context_window": 65536,
     "thinking": "off",
     "style": "default"
   }
   ```
   > ⚠️ **Lưu ý**: `context_window` trong `config.json` **bắt buộc phải khớp** với `num_ctx` đã đặt trong Modelfile của Ollama.

---

### Cách 2: Dùng Cloud API (OpenRouter, Gemini, Claude, OpenAI, DeepSeek)

#### Ví dụ OpenRouter:
```json
{
  "language": "vi",
  "provider": "openrouter",
  "model": "anthropic/claude-3.5-sonnet",
  "providers": {
    "openrouter": {
      "api_key": "sk-or-v1-YOUR_OPENROUTER_API_KEY"
    }
  },
  "context_window": 128000,
  "thinking": "off",
  "style": "default"
}
```

#### Ví dụ Google Gemini:
```json
{
  "language": "vi",
  "provider": "gemini",
  "model": "gemini-2.5-pro",
  "providers": {
    "gemini": {
      "api_key": "YOUR_GEMINI_API_KEY"
    }
  },
  "context_window": 1000000,
  "style": "default"
}
```

#### Ví dụ DeepSeek API:
```json
{
  "language": "vi",
  "provider": "deepseek",
  "model": "deepseek-chat",
  "providers": {
    "deepseek": {
      "api_key": "YOUR_DEEPSEEK_API_KEY"
    }
  },
  "context_window": 64000,
  "style": "default"
}
```

---

### Cách 3: Phối hợp nhiều Model theo vai trò (Tối ưu chi phí & chất lượng)
Hệ thống cho phép gán model mạnh (Claude 3.5 Sonnet / DeepSeek Reasoner) làm **Biên tập / Kiến trúc sư** và model nhanh/rẻ (DeepSeek Chat / Ollama) làm **Người viết**:

```json
{
  "language": "vi",
  "provider": "ollama",
  "model": "ainovel-qwen",
  "providers": {
    "ollama": { "base_url": "http://host.docker.internal:11434/v1" },
    "openrouter": { "api_key": "sk-or-v1-YOUR_KEY" }
  },
  "roles": {
    "architect": { "provider": "openrouter", "model": "anthropic/claude-3.5-sonnet" },
    "editor": { "provider": "openrouter", "model": "anthropic/claude-3.5-sonnet" },
    "writer": { "provider": "ollama", "model": "ainovel-qwen" }
  },
  "context_window": 65536,
  "style": "default"
}
```

---

## 5. Bắt Đầu Sáng Tác

### Giao diện TUI trực quan

Chạy bằng Docker Compose:
```bash
docker compose run --rm ainovel
```

- **Phím Tab**: Chuyển đổi giữa 2 chế độ khởi động:
  - **Bắt đầu nhanh**: Nhập 1 câu tóm tắt ý tưởng, AI tự động lên dàn ý và viết ngay.
  - **Đồng sáng tác**: AI sẽ trao đổi cùng bạn từng bước để làm rõ thiết lập thế giới, nhân vật, cốt truyện trước khi viết.
- **Phím Enter**: Bắt đầu quá trình sáng tác.
- **Phím `/`**: Mở thanh tìm kiếm lệnh nhanh.
- **`Ctrl+C` 2 lần**: Lưu an toàn và thoát ra.

### Chế độ Headless (Chạy ngầm server)

Dành cho người muốn chạy tự động trên VPS/Server:
```bash
# Bắt đầu truyện mới
docker compose run --rm ainovel --headless --prompt "Tiểu thuyết tu tiên phàm nhân, nhân vật chính cẩn trọng cơ trí"

# Viết tiếp truyện đang dở (không truyền --prompt)
docker compose run --rm ainovel --headless
```

---

## 6. Quản Lý Sáng Tác Nhiều Truyện Độc Lập

Mặc định output sẽ lưu vào thư mục `workspace/`. Để viết nhiều bộ truyện khác nhau mà không bị xung đột, hãy đặt biến môi trường `NOVEL_DIR`:

```powershell
# Trên Windows PowerShell:
$env:NOVEL_DIR = ".\novels\tien-hiep-ky"
docker compose run --rm ainovel

# Viết bộ truyện khác:
$env:NOVEL_DIR = ".\novels\do-thi-di-nang"
docker compose run --rm ainovel
```

Cấu trúc thư mục đầu ra của mỗi truyện:
```text
novels/<tên-truyện>/output/novel/
├── chapters/            # Các chương hoàn chỉnh đã duyệt (.md)
├── drafts/              # Bản nháp và dàn ý chi tiết từng chương
├── reviews/             # Báo cáo đánh giá của Editor
├── summaries/           # Tóm tắt từng Cung và Tập
├── premise.md           # Ý tưởng & tiền đề cốt truyện
├── characters.md        # Hồ sơ thiết lập nhân vật
├── world_rules.md       # Thiết lập quy tắc thế giới
└── meta/                # Checkpoint, tiến độ, nhật ký token
```

---

## 7. Bảng Lệnh Điều Khiển Trong TUI (Slash Commands)

Khi đang ở trong giao diện TUI, bạn có thể gõ `/` để mở menu lệnh:

| Lệnh | Mô tả |
| :--- | :--- |
| `/help` | Mở bảng trợ giúp tra cứu danh sách lệnh và phím tắt |
| `/model` | Chuyển đổi Model hoặc mức độ suy luận (reasoning/thinking) |
| `/config` | Quản lý cấu hình Provider, Model ID, API key, Base URL |
| `/diag` | Xem báo cáo chẩn đoán toàn diện về sức khỏe, tiến độ và chất lượng truyện |
| `/review` | Bật/tắt chế độ duyệt từng chương (dừng lại sau mỗi chương để bạn duyệt) |
| `/next` | Phê duyệt cho phép viết chương tiếp theo (khi ở chế độ duyệt) |
| `/start <tệp>` | Đọc tệp dàn ý / ý tưởng bên ngoài để bắt đầu truyện mới |
| `/import <tệp>` | Nhập tiểu thuyết từ bên ngoài vào để AI phân tích và viết tiếp |
| `/reopen <hướng>` | Viết tiếp tập mới sau khi tác phẩm đã hoàn thành |
| `/cocreate` | Tạm dừng để vào chế độ đồng sáng tác định hướng giai đoạn tiếp theo |
| `/simulate` | Phân tích các file văn mẫu trong `./simulate` để mô phỏng văn phong |
| `/importsim <file>` | Nhập hồ sơ mô phỏng văn phong từ tệp json |
| `/sync` | Đồng bộ các chỉnh sửa thủ công của bạn trên các file chương vào hệ thống |
| `/export` | Xuất tác phẩm thành file văn bản hoàn chỉnh (.txt) |

> 💡 **Can thiệp thời gian thực**: Bạn có thể nhập thẳng ý kiến sửa đổi vào ô chat bất cứ lúc nào (ví dụ: *"Ở chương 15 cho nhân vật phụ A hy sinh để tạo bước ngoặt"*), Coordinator sẽ tự động tiếp nhận và điều phối vào mạch truyện.

---

## 8. Xuất Truyện (TXT / EPUB)

Trong TUI, gõ `/export` hoặc chạy lệnh:
- `/export [đường_dẫn] [from=N] [to=M] [--overwrite]`
- Ví dụ: `/export novels/xuat_ban.txt from=1 to=50`

---

## 9. Quy Tắc & Lưu Ý Quan Trọng

1. ⚠️ **Quy tắc 1 engine / 1 truyện**: Không mở cùng lúc 2 cửa sổ TUI trên cùng một thư mục truyện để tránh xung đột ghi đè dữ liệu.
2. ⚠️ **Khớp ngữ cảnh (`num_ctx` == `context_window`)**: Nếu dùng Ollama, luôn đảm bảo `num_ctx` trong Modelfile bằng chính xác giá trị `context_window` trong file config.
3. 🔒 **Bảo mật**: File `config/config.json`, thư mục `novels/`, và `workspace/` đã được cấu hình trong `.gitignore` để không bị lộ API key hay bản thảo riêng tư khi push lên GitHub.
