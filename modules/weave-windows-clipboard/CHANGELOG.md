# Changelog

## [0.2.6](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/modules/weave-windows-clipboard/v0.2.5...modules/weave-windows-clipboard/v0.2.6) (2026-10-06)


### Bug Fixes

* stage clipboard files on disk, in a directory of the user's own ([#29](https://github.com/weaveplatform/weaveplatform-agent-modules/issues/29)) ([2bac1a4](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/2bac1a45ba06e9d3b651890bd22d9b3eb36dbd39))

## [0.2.5](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/modules/weave-windows-clipboard/v0.2.4...modules/weave-windows-clipboard/v0.2.5) (2026-10-06)


### Features

* stream clipboard copies of any size between host and guest ([#27](https://github.com/weaveplatform/weaveplatform-agent-modules/issues/27)) ([0c9536b](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/0c9536b6738897cf4d7094397411c73f158affe1))

## [0.2.4](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/modules/weave-windows-clipboard/v0.2.3...modules/weave-windows-clipboard/v0.2.4) (2026-10-06)


### Bug Fixes

* **modulezip:** install over read-only files ([2a17e11](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/2a17e1100e9a8bd389b9ce2cdbb55a10e896099d))
* **weave-windows-clipboard:** carry bitmap images both ways ([dd4e0df](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/dd4e0df4b3e2454d27c1b237c8b6a2ac6d1a99e0))
* Windows bitmap images, sized clipboard stats and read-only module installs ([e6aac85](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/e6aac8551103bb4b43a739379d4a2c8836efaebd))

## [0.2.3](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/modules/weave-windows-clipboard/v0.2.2...modules/weave-windows-clipboard/v0.2.3) (2026-10-06)


### Bug Fixes

* **sdk:** let a module serve its pipe as a non-admin user ([f398e3f](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/f398e3facb680c534bd0b934f5d1eaa77cb76c2f))
* **weave-windows-clipboard:** start in the console user's session ([3cd60b8](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/3cd60b8280eebeec6ef6e7fad79f26252f5de436))

## [0.2.2](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/modules/weave-windows-clipboard/v0.2.1...modules/weave-windows-clipboard/v0.2.2) (2026-10-05)


### Features

* sign and package Windows modules ([aed3ee7](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/aed3ee7a2e7c9802acfbb6ccba4aacb32d0557bb))


### Bug Fixes

* pin the weaveplatform code-signing certificate in Windows manifests ([1a53ae2](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/1a53ae2bbc9887ed55836068b931584c30542de3))

## [0.2.1](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/modules/weave-windows-clipboard/v0.2.0...modules/weave-windows-clipboard/v0.2.1) (2026-10-04)


### Features

* **clipboard:** carry PDF on Windows ([8507ca1](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/8507ca11fa3faf4c0fe4422a6e6b6553fb4c459f))
* **clipboard:** report what each guest clipboard holds ([3dc620b](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/3dc620b0929bf01a9c8104a8186a3f214d61bb24))
* macOS module packages and clipboard parity across guest OSes ([8d95aac](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/8d95aace425d6ec366a759250289cc877e149d05))


### Bug Fixes

* **clipboard:** read the CF_HTML fragment by byte offset on Windows ([7ade67f](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/7ade67f14871c209fc78f45f9ae28e7095cc8875))

## 0.2.0 (2026-10-03)


### Features

* capability modules for linux, macos and windows ([2b54757](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/2b54757324e07769f710b7a76da8978ed516b81c))
* module sdk and capability modules ([ae06c6b](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/ae06c6b202585d8097654e2d4ffffd89063818eb))
