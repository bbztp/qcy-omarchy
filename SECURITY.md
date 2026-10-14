# Security policy

## Reporting a vulnerability

Please use GitHub private vulnerability reporting (the "Report a vulnerability"
button under the repository's Security tab). Do not open a public issue.

## Scope

- the daemon (`qcyctld`), the CLI (`qcyctl`) and the shell plugin;
- the install and bootstrap scripts.

Firmware/OTA writing is forbidden by design; any path that writes firmware is
a vulnerability and should be reported.
