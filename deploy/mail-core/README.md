# Stalwart mail-core functional PoC

This directory is an isolated protocol harness. It does not modify the shared
server Compose deployment, public DNS, MX records, MailHub, or real mail data.

The server is Stalwart Community `v0.16.19` for `linux/amd64`, pinned to
`sha256:34e59515ef633e2353abbd9a12b0a232eb2174df9ac1ee820ed13e952cfd801d`.
The management client is `stalwart-cli 1.0.12`, pinned to
`sha256:8831cc276c22e334bcf473fc7d28e21248d748c4bc1dac43c83738d3cbd3a9d5`.

Stalwart Community is offered under AGPL-3.0; the repository also describes a
commercial SELv2 option. This PoC is evaluation evidence, not production
license approval.

Run the complete Windows verification from a clean PoC project:

```powershell
powershell.exe -NoProfile -File deploy/mail-core/verify.ps1
```

The script generates credentials in a restricted, Git-ignored directory under
`.tmp/`. It passes CLI credentials through an environment file and sends JMAP
authorization in-process, so credentials do not appear in Docker/curl command
arguments. The long-running Stalwart container is recreated without the
recovery credential before it can be retained.

The harness starts only loopback high ports and validates:

- declarative bootstrap with the reserved `mail-suite.test` domain;
- unknown-recipient and open-relay rejection;
- active-recipient `RCPT TO` and `DATA` acceptance;
- JMAP `Email/query`, structured Message-ID/From/To/text/attachment checks, and
  attachment/raw-blob downloads for the delivered fixture;
- raw-blob SHA-256 stability after restarting Stalwart;
- account create, disable, enable, delete, and create again without
  historical-success reuse.

By default the script removes its project containers, network, and volumes in
a `finally` block. Pass `-Keep` to retain the isolated environment for manual
inspection; the restricted credential directory is retained with it and its
path is reported without printing credential values. A later run never deletes
that state implicitly. Use the explicit `-Reset` switch to print the exact
project/volume boundary and remove existing PoC state before starting:

```powershell
powershell.exe -NoProfile -File deploy/mail-core/verify.ps1 -Reset
```

Retained Docker state can also be removed explicitly with:

```powershell
docker compose --project-name mail-suite-mail-core-poc `
  -f deploy/mail-core/compose.yaml down --volumes --remove-orphans
```

After the Docker state is gone, remove the three known credential files and
then the empty restricted directory:

```powershell
Remove-Item -LiteralPath .tmp/mail-core-poc-secrets/compose.env
Remove-Item -LiteralPath .tmp/mail-core-poc-secrets/cli.env
Remove-Item -LiteralPath .tmp/mail-core-poc-secrets/credentials.env
Remove-Item -LiteralPath .tmp/mail-core-poc-secrets
```

This is a single Stalwart instance with local RocksDB. Even if it passes every
check, it is not multi-node HA. Shared PostgreSQL/object storage, two protocol
instances, per-entry SMTP probes, and fault-domain injection remain separate
acceptance gates. Wrong-credential and induced-timeout failures also remain
pending. A normal restart proves only basic local-volume persistence; SMTP
`250` crash durability remains pending until an immediate post-`250`
termination and cross-fault-domain read test passes.

Port overrides are read by both Compose and the verification client. Set
`MAIL_CORE_SMTP_PORT`, `MAIL_CORE_HTTPS_PORT`, and
`MAIL_CORE_RECOVERY_PORT` before invoking the script. When
`MAIL_CORE_PUBLIC_URL` is set, its HTTPS port must match
`MAIL_CORE_HTTPS_PORT`.
