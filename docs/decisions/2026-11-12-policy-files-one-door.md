# Policy files are edited through mesh7, by one door

- **Problem**: a policy could be written two ways. The per-tool edit went through mesh7 (validated, backed up, traced); the console's YAML editor wrote the file on disk itself, with no validation before the write, no trace, and a console bound to the mesh's own machine.
- **Decision**: mesh7 serves the files of `policy_dir`: `GET /policies/files`, `GET /policies/files/{name}` (text, fingerprint in `ETag`) and `PUT /policies/files/{name}` (`If-Match` required). A write takes the path of the per-tool edit: one at a time, previous version in `.bak`, whole set validated, file restored on refusal, set applied at once, edit recorded as `mesh.policy_file_edit`. Only an existing file is replaced.
- **Why**: a governance proxy cannot let its own rules change outside its journal. One door also lets the console and a CLI be two clients of the same API, and lets the console run on another host.
- **Where**: `proxy/policy_files.go`, routing in `proxy/handler.go`; tests in `proxy/policy_files_test.go`. Console side: `flux7-console`, `lib/api/mesh.ts` (the `api/policy-files` routes are removed).
