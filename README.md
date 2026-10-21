# fitgirl-dl

Gets the direct download links from a FitGirl repack page, and can download the files too.

**Last tested and working:** 2026-10-21 13:46 (UTC+03:30)

## Why

I used to maintain **dlfit**, a terminal download manager like IDM with a plugin system. Its FitGirl plugin was the only reason people used it. When FuckingFast changed its pages and added Cloudflare protection, the plugin broke, and keeping the whole app and its plugins updated wasn't worth it, so I abandoned dlfit. It's no longer online.

fitgirl-dl is a much smaller replacement that's easy to fix. I don't promise to follow every FuckingFast change, but I download from FitGirl once in a while. If it breaks then, I'll fix it and update the date above.

## Usage

```sh
fitgirl-dl <url>                     print the direct links
fitgirl-dl <url> --download          download to ~/Downloads/<content-name>
fitgirl-dl <url> --download <path>   download into <path>
```

You're probably better off without `--download`: just get the links and paste them into IDM or your favorite download manager. I love the terminal too much not to add it, though, so feel free to use it. It downloads 4 files at a time with 8 connections per file, and skips files that are already complete.

Requires Google Chrome; if it's missing, Chromium is downloaded on the first run.

## What it relies on

If it breaks, one of these probably changed:

- FitGirl pages load with plain HTTP, with no stealth needed.
- FuckingFast pages get past Cloudflare with a stealth headless Chrome.
- DOWNLOAD needs two clicks: the first opens an ad, and the second returns the direct link in an `HX-Redirect` header.
- One page at a time with a 1s pause stays under the rate limit.
- Direct links need no cookies and support range requests.
