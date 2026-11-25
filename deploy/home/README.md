Runtime-private agent configuration, copied into the image read-only at
`/etc/multica-sandbox/home-template`. Only the Claude skills under
`.claude/skills/` are copied into HOME, by the entrypoint at container start.
Nothing else here is copied into HOME. Any other file reaches an agent only
through a setting that names it, for example `OPENCODE_CONFIG` for
`.config/opencode/opencode.json` (set in `Dockerfile.pod`).

Do not put secrets here.
