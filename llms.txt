# Strife Server Documentation

Strife is a lightweight, host-based web server that blends static file serving with a powerful Lua scripting engine. It allows for dynamic request handling, database integration, and system-level operations via Lua, while maintaining a strict directory-based routing system.

## Core Architecture

### Host-Based Routing
Strife uses the `Host` header of incoming requests to determine the root directory for the request.
- **Root Directory**: Defined by `STRIFE_ROOT`.
- **Resolution**: If a request comes in for `example.com`, the server looks for files in `{STRIFE_ROOT}/example.com/`.
- **Path Matching**: 
    - Supports exact directory/file matches.
    - Supports **Wildcards**: Directories named `_` act as catch-all components in the URI path.
    - **Index Files**: If a directory is requested, the server prioritizes `index.lua`, then `index.html`.

### Security Model
- **Hidden Components**: Any path containing a component starting with `.` (excluding `.well-known`) is forbidden to prevent access to `.git`, `.env`, etc.
- **Symlinks**: The server evaluates symlinks and re-verifies that the resolved path does not escape the host root or enter hidden directories.
- **Request Limits**: Default maximum request body size is 10MB.

## Configuration

Configuration is handled via environment variables or `.env` files (passed via `--dotenv`).

| Variable | Default | Description |
| :--- | :--- | :--- |
| `STRIFE_PORT` | `8080` | The TCP port the server listens on. |
| `STRIFE_ROOT` | `.` | The base directory containing host folders. |
| `STRIFE_SQL` | `""` | Path to a `.sql` file to initialize the in-memory SQLite database. |

## Lua Scripting API

When a `.lua` file is executed, it has access to a global `strife` table. Scripts have a 30-second execution timeout.

### `strife.request`
Information about the incoming HTTP request.
- `method`: HTTP method (GET, POST, etc.).
- `uri`: The original request URI.
- `path`: The URL path.
- `host`: The cleaned host header.
- `remote_addr`: Client IP and port.
- `headers`: Table of lowercase header keys to values.
- `query`: Table of query parameters (values are arrays).
- `body`: The raw request body as a string.

### `strife.response`
Used to control the HTTP response.
- `status`: (Number) Set the HTTP status code (default: 200).
- `headers`: (Table) Set response headers.
- `body`: (String) Append content to the response body.
- `writeBlob(data)`: Immediately sends headers and writes raw binary data to the wire.

### `strife.file`
Filesystem operations relative to the host root.
- `read(path)`: Reads file content as a string.
- `write(path, content)`: Writes content to a file.
- `move(src, dest)`: Renames/moves a file.
- `list(path)`: Returns a table of entries. Each entry contains:
    - `name`, `path` (relative to host root), `is_dir`, `size`, `mod_time`.

### `strife.db`
Interface to the shared in-memory SQLite database.
- `query(sql, ...args)`: Executes a query. 
    - To pass binary data, use a table: `{blob = "..."}`.
    - Returns a table of rows (each row is a table of column names to values).

### `strife.http`
Outbound HTTP capabilities.
- `request(options)`: Performs an HTTP request.
    - Options table: `{url, method, headers, body}`.
    - Returns: `{status, headers, body}`.
- `reverse_proxy(url)`: Proxies the current request to the specified URL.

### `strife.os`
System interaction.
- `exec(command, args_table)`: Runs a system command.
    - Returns: `{stdout, stderr, code}` and an error message if applicable.

### `strife.crypto`
Security primitives.
- `genUUID()`: Generates a version 4 UUID.
- `getOTP(secret)`: Generates a TOTP (6-digit) code from a base32 secret.
- `hashPassword(password)`: Returns an Argon2id hash string (standard format).

### `strife.json` & `strife.template`
- `json.encode(val)` / `json.decode(str)`: Standard JSON processing.
- `template.parse(tmplStr, dataTbl)`: Processes a Go-style text template using the provided Lua table as data.

## Logging
The server outputs structured JSON logs to stdout:
- **Levels**: `INFO`, `ERROR`.
- **Topics**: `SERVER`, `REQUEST`, `LUA`, `DATABASE`.
- **Lua Logging**: Use `strife.log(details, level)` within scripts.
