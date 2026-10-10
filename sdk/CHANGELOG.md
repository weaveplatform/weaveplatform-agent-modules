# Changelog

## [0.2.8](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/sdk/v0.2.7...sdk/v0.2.8) (2026-10-09)


### Bug Fixes

* retain core degradation in SDK registry snapshots ([#37](https://github.com/weaveplatform/weaveplatform-agent-modules/issues/37)) ([d09af72](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/d09af7295293fbfb16af4ea3bc71d2f0a7fe989f))

## [0.2.7](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/sdk/v0.2.6...sdk/v0.2.7) (2026-10-06)


### Bug Fixes

* stage clipboard files on disk, in a directory of the user's own ([#29](https://github.com/weaveplatform/weaveplatform-agent-modules/issues/29)) ([2bac1a4](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/2bac1a45ba06e9d3b651890bd22d9b3eb36dbd39))

## [0.2.6](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/sdk/v0.2.5...sdk/v0.2.6) (2026-10-06)


### Features

* stream clipboard copies of any size between host and guest ([#27](https://github.com/weaveplatform/weaveplatform-agent-modules/issues/27)) ([0c9536b](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/0c9536b6738897cf4d7094397411c73f158affe1))

## [0.2.5](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/sdk/v0.2.4...sdk/v0.2.5) (2026-10-06)


### Bug Fixes

* **sdk:** end testkit host streams with the caller's context status ([deb894b](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/deb894b17cbce7ae9513a46b6a63128a0db8025d))
* **sdk:** size every format a clipboard stat lists ([0d5d0f9](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/0d5d0f995c3612d8659e8b80cd9334b14d1fdec4))
* session module binding race and testkit stream status ([d50eff7](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/d50eff719a9c1cb0b0a691d1990aa791aa523906))
* Windows bitmap images, sized clipboard stats and read-only module installs ([e6aac85](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/e6aac8551103bb4b43a739379d4a2c8836efaebd))

## [0.2.4](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/sdk/v0.2.3...sdk/v0.2.4) (2026-10-06)


### Bug Fixes

* **sdk:** let a module serve its pipe as a non-admin user ([f398e3f](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/f398e3facb680c534bd0b934f5d1eaa77cb76c2f))
* **sdk:** let a module serve its pipe as a non-admin user ([b1a4c10](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/b1a4c10a6c520a75f99c4e385957c01c437e6cff))

## [0.2.3](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/sdk/v0.2.2...sdk/v0.2.3) (2026-10-05)


### Bug Fixes

* **sdk:** retry busy exec input and tell a stalled channel from an old core ([6102d53](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/6102d537ce54e99e10c95f04e5c054aacdff69ab))
* **sdk:** retry exec input past a busy module instead of cutting it off ([6e35ffa](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/6e35ffa476ef494cdcadc8b4b05285284bf5ae77))
* **sdk:** tell a stalled channel from a core without a registry ([a9bfd85](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/a9bfd85fe5bd4c540d841919f3367cdb407675e7))

## [0.2.2](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/sdk/v0.2.1...sdk/v0.2.2) (2026-10-04)


### Features

* **clipboard:** report what each guest clipboard holds ([3dc620b](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/3dc620b0929bf01a9c8104a8186a3f214d61bb24))
* macOS module packages and clipboard parity across guest OSes ([8d95aac](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/8d95aace425d6ec366a759250289cc877e149d05))

## [0.2.1](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/sdk/v0.2.0...sdk/v0.2.1) (2026-10-04)


### Features

* **sdk:** module registry and undeliverable-message errors ([80cc770](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/80cc77088cc739bd30d889fda84712f57f17e4f6))
* **sdk:** module registry and undeliverable-message errors in weaveclient ([3d9389d](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/3d9389dc2c2c46cb236aef5937b480be71c65dcd))
* **sdk:** registry host service for modules ([d5109f2](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/d5109f27c793ae4d9e7f08292b5c06516e8b19cc))

## 0.2.0 (2026-10-03)


### Features

* module sdk and capability modules ([ae06c6b](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/ae06c6b202585d8097654e2d4ffffd89063818eb))
* **sdk:** module sdk ([a27868f](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/a27868f1189180fc82a6255e3733fb437bf85965))


### Bug Fixes

* **sdk:** generate the protocol from weave-agent v0.9.0 ([9e31a1f](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/9e31a1fab6d872653e12d49d0ef8cc34206d21f8))
* **sdk:** generate the protocol from weave-agent v0.9.0 ([e1a1e49](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/e1a1e49f54e8af77c0a646bf1c8a28827ef0427b))
