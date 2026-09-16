# Changelog

## Unreleased

### Added

- Optional Lume macOS backend with identity-pinned clones, durable sandbox
  metadata, snapshot recovery, SSH guest execution, and SFTP file transfer.
- Mac platform dispatch and the Tahoe desktop seed catalog entry, qualified
  through deployed MCP on `agentcompute01`.

### Fixed

- Negotiate SSH host-key algorithms from the pinned known-host entries.
- Omit unchanged disk sizes from Lume resource updates.
- Run host scripts under `/bin/sh` so empty sandbox discovery works with a
  zsh login shell.

## [0.1.3](https://github.com/GilmanLab/agentcompute/compare/v0.1.2...v0.1.3) (2026-09-16)


### Bug Fixes

* **lume:** enforce disabled VNC with an account-local pinned build ([#39](https://github.com/GilmanLab/agentcompute/issues/39)) ([9a9e259](https://github.com/GilmanLab/agentcompute/commit/9a9e259bb03d644b8a4142437b4d0492aa70bb00))

## [0.1.2](https://github.com/GilmanLab/agentcompute/compare/v0.1.1...v0.1.2) (2026-09-16)


### Features

* **desktop:** expose native Driver calls and screenshot URLs ([#26](https://github.com/GilmanLab/agentcompute/issues/26)) ([fa80821](https://github.com/GilmanLab/agentcompute/commit/fa80821970fb2da631fad467462838f89f279f28))
* **images:** promote qualified lab image digests ([#30](https://github.com/GilmanLab/agentcompute/issues/30)) ([4c0b0d2](https://github.com/GilmanLab/agentcompute/commit/4c0b0d21218f5e607733ec277d1fb98053bdcb5e))
* **lume:** qualify macOS backend against deployed service ([#36](https://github.com/GilmanLab/agentcompute/issues/36)) ([7f72d09](https://github.com/GilmanLab/agentcompute/commit/7f72d09e3f60ba9230258cdc833a12adeee76cfa))
* **macos:** qualify the Tahoe Lume seed and its Mac Studio host ([#29](https://github.com/GilmanLab/agentcompute/issues/29)) ([01e0e06](https://github.com/GilmanLab/agentcompute/commit/01e0e06b1dc86288217013a8f63f0a8d104d8e12))

## [0.1.1](https://github.com/GilmanLab/agentcompute/compare/v0.1.0...v0.1.1) (2026-09-15)


### Bug Fixes

* **http:** support authenticated loopback reverse proxies ([#34](https://github.com/GilmanLab/agentcompute/issues/34)) ([bd80c97](https://github.com/GilmanLab/agentcompute/commit/bd80c97f981af3bcd7c8fafc02ead4802ac0978c))

## 0.1.0 (2026-09-15)


### Features

* **auth:** verify named bearer credentials for HTTP ([#32](https://github.com/GilmanLab/agentcompute/issues/32)) ([2c7376d](https://github.com/GilmanLab/agentcompute/commit/2c7376d67db7578376d6e3c6a3177646eda2e49a))
* **compute:** implement Incus container sandbox slice ([#14](https://github.com/GilmanLab/agentcompute/issues/14)) ([358c572](https://github.com/GilmanLab/agentcompute/commit/358c5724bbe43a8bbfb02ff060fadec6c063bd2a))
* **images:** add router recipe, pins, publish pipeline, and import spike ([#7](https://github.com/GilmanLab/agentcompute/issues/7)) ([0b71543](https://github.com/GilmanLab/agentcompute/commit/0b715439826b9f59c16c1294eae65d8b06930e5c))
* **images:** bake the router NAT helper ([#20](https://github.com/GilmanLab/agentcompute/issues/20)) ([5de3ddb](https://github.com/GilmanLab/agentcompute/commit/5de3ddb269b80d5c70e8f51e8314e6537dcb5169))
* **images:** build and qualify the Ubuntu X11 desktop VM ([#25](https://github.com/GilmanLab/agentcompute/issues/25)) ([e4333f2](https://github.com/GilmanLab/agentcompute/commit/e4333f245b4e81c8d7753038f0ddf04a620bd0a2))
* **images:** build runner VMs and dispatch bakes to a private repository ([#17](https://github.com/GilmanLab/agentcompute/issues/17)) ([0cafbec](https://github.com/GilmanLab/agentcompute/commit/0cafbec6cd9ec875c6d41efc40355985dd05300a))
* **images:** promote qualified lab image digests ([#19](https://github.com/GilmanLab/agentcompute/issues/19)) ([a0d6860](https://github.com/GilmanLab/agentcompute/commit/a0d68606c8bcb17313274ea2f9865969e6f3b9a6))
* **images:** record the published router release in the catalog ([#13](https://github.com/GilmanLab/agentcompute/issues/13)) ([bc6e754](https://github.com/GilmanLab/agentcompute/commit/bc6e754c6cbe0d807ec89261cac35e1936bb2711))
* **incus:** complete sandbox networking and lifecycle ([#22](https://github.com/GilmanLab/agentcompute/issues/22)) ([208e15c](https://github.com/GilmanLab/agentcompute/commit/208e15c78ca6b07357c170ceea71b8b635c033fb))
* **windows:** qualify cluster-local Windows images end to end ([#28](https://github.com/GilmanLab/agentcompute/issues/28)) ([0611f31](https://github.com/GilmanLab/agentcompute/commit/0611f311adcb6a21439e473cd03d2a51cb0e580d))


### Bug Fixes

* **images:** assemble with umask 022 so generated files are not 0600 ([#12](https://github.com/GilmanLab/agentcompute/issues/12)) ([4059565](https://github.com/GilmanLab/agentcompute/commit/4059565b81b16311c11c4b84edfe1b1cfb3aa614))
* **images:** compare the pinned Incus client version exactly ([#9](https://github.com/GilmanLab/agentcompute/issues/9)) ([ba6305f](https://github.com/GilmanLab/agentcompute/commit/ba6305fca7adc7914854234e0caac5e794bff63c))
* **images:** drop duplicate --accept-routes from tailnet enrollment ([#10](https://github.com/GilmanLab/agentcompute/issues/10)) ([bab7107](https://github.com/GilmanLab/agentcompute/commit/bab710717b1dcb3c97c4868ead1d1c6a5c98ee45))
* **images:** include distrobuilder VM dependency in publisher ([#18](https://github.com/GilmanLab/agentcompute/issues/18)) ([a3db6b4](https://github.com/GilmanLab/agentcompute/commit/a3db6b4ec7d8e8d54841f6261c86c25720fa4a30))
* **images:** keep catalog updates from republishing and fix alias promotion ([#11](https://github.com/GilmanLab/agentcompute/issues/11)) ([8fcc4f7](https://github.com/GilmanLab/agentcompute/commit/8fcc4f7262d3849f66c5cefaaa76f701c97c99a6))

## Changelog
