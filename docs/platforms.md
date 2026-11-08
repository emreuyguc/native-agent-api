# Platform support

Verified on macOS arm64 (full checks) and in the Linux arm64 Docker images of both agents
(`docker/verify.sh`, with and without restricted egress).

Native Windows is not supported: process groups use Unix system calls. WSL and other Linux
environments behave like Linux but are not verified release targets.

CLI availability and login storage are platform-specific (for example the macOS Keychain). The CLI
gets only `HOME`, `USER`, `PATH`, `TMPDIR` and the optional proxy variables from the environment.
