# Third-party notices

Launchpad's own code is under the [LICENSE](LICENSE) of this repository. The
files below are by other authors and keep their own licences.

## Web fonts (`web/src/assets/fonts/`)

The Paper and Terminal themes (LAUN-235) use four font families. The web app
bundles them as `woff2` files (latin and latin-ext subsets, taken from the
[fontsource](https://fontsource.org) npm packages named below) and serves them
itself: a browser never asks a third party for them.

All four are under the SIL Open Font License, Version 1.1. The full licence
text with each copyright notice is next to the files.

| Family | Weights and styles | Copyright | Licence text | Source package |
|---|---|---|---|---|
| Fraunces | 600, 600 italic | Copyright 2020 The Fraunces Project Authors (github.com/undercasetype/Fraunces) | `OFL-Fraunces.txt` | `@fontsource/fraunces` 5.3.0 |
| Source Sans 3 | 400, 600 | Copyright 2010-2020 Adobe (http://www.adobe.com/), with Reserved Font Name 'Source' | `OFL-SourceSans3.txt` | `@fontsource/source-sans-3` 5.3.1 |
| IBM Plex Mono | 400 | Copyright 2017 IBM Corp. | `OFL-IBMPlexMono.txt` | `@fontsource/ibm-plex-mono` 5.3.0 |
| JetBrains Mono | 400, 700 | Copyright 2020 The JetBrains Mono Project Authors (https://github.com/JetBrains/JetBrainsMono) | `OFL-JetBrainsMono.txt` | `@fontsource/jetbrains-mono` 5.3.0 |

The files are not changed. Reserved Font Names are not used for any changed
version.
