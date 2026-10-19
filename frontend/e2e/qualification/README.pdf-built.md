# R2 built PDF qualification (prepared, not run)

This is a separate, mandatory built-app check for the preparatory R2 source. It
does not replace the ordinary frontend, browser, dependency, registry, Cloud or
release gates. Rebase R2 onto the actual accepted R1 parent before qualification.
The original R2 preparation and the original Claude checkouts are unchanged.

The test configuration deliberately has no server command, global setup or
dependency-install step. The root operator must supply an owned loopback Go app
whose native binary and embedded built assets are bound to the exact candidate,
with the committed `SecurityHeaders` middleware. A URL alone is not proof of
that binding. A Vite development or preview server is not a substitute.

After that server and its cleanup contract have been reviewed, run from
`frontend` using the existing locked dependencies and approved Node/Chromium:

```powershell
$env:R2_PDF_BASE_URL = 'http://127.0.0.1:<owned-port>'
npx playwright test --config=playwright.pdf-built.config.ts
```

The dedicated configuration selects exactly two tests, one worker, no retries
and a 120-second suite deadline. They are outside `e2e/tests`; ordinary suite
success therefore does **not** imply these tests ran. Preserve the exact test
inventory, attempt/result records, attachment, server/build identity and owned
process cleanup when the operator eventually runs them. This preparation has
not installed dependencies, built an app, launched a browser or run these tests.

## What actually executes in the first test

API responses contain only original synthetic identities and PDF bytes. No
renderer, Worker, canvas, emitted script, font, CMap or decoder is mocked.
Blob URL observers call the original functions. The real app opens its protected
media UI, dynamically loads pdfjs, creates its Worker and renders two pages.

| Input and assertion | Runtime asset required |
| --- | --- |
| Named CJK encoding in the PDF; browser/worker fetch and exact package bytes | `cmaps/UniJIS-UCS2-H.bcmap` |
| Actual Symbol and ZapfDingbats glyph ink plus fetched package bytes | `standard_fonts/FoxitSymbol.pfb`, `standard_fonts/FoxitDingbats.pfb` |
| CCITT Group 3 image; left-white/right-black pixels | `wasm/jbig2_nowasm_fallback.js` |
| Lossless JPX image; left-green/right-magenta pixels | `wasm/openjpeg_nowasm_fallback.js` |
| Real Worker lifecycle and exact served/package bytes | hashed `legacy/build/pdf.worker.min.mjs` asset |

In pdfjs 6.3.289 the JBIG2/CCITT class uses the first JavaScript module; the JPX
class uses the second. The committed R2 Vite plugin emits these two decoder
modules. `openPdfPreview` sets `useWasm: false`; the test requires no `.wasm`
request and the exact production CSP without `unsafe-eval` or
`wasm-unsafe-eval`. It requires no new CSP violation during PDF use, and records
any existing application bootstrap violations separately rather than claiming
the whole application is violation-free.

The CCITT sample exercises the shared decoder through **CCITTFaxDecode**, not a
JBIG2 bitstream. JPXDecode exercises the OpenJPEG fallback. No ICC/qcms or WASM
execution is claimed: qcms is not an emitted runtime asset in this Vite plugin.
One named CMap is exercised, not every CMap/language. All emitted CMap/license
inventory remains part of the exact built-asset inspection; this test's selected
runtime checks are not an exhaustive codec or font conformance suite.

The first test also checks opaque download bytes and filename, no PDF iframe or
new tab, page-two rendering, the canvas cap, Escape-driven Worker termination,
and a new real Worker on reopen without refetching authorized media. Asset GETs
performed by the test itself cannot satisfy the browser/worker decoder-fetch
assertions. All pixel oracles must pass; a page marked rendered alone is
insufficient.

## Workspace boundary

The second test holds the selected workspace's media response, uses the real
workspace selector, observes the document reload and `net::ERR_ABORTED`, then
waits for a successful transcript GET carrying the new workspace header. Only
then does it release the old synthetic response and await that route's
settlement. The old message, preview and blob must be absent. Arbitrary route
errors are not swallowed; only the independently observed canceled request may
fail fulfillment. This is a real **reload** boundary; it does not claim a
same-document prop-change or logout/cross-tab proof. Existing unit coverage for
those lifecycles and actual-parent qualification remain required.

## Fixture provenance and limits

`synthetic-preview-pdf.ts` computes byte offsets and xref entries for an original
two-page PDF. Its CCITT bytes are eight identical, byte-aligned rows: white run
4 (`1011`), black run 4 (`011`), padding 0. The embedded 294-byte JP2 has SHA256
`7d2278c692e8eb08410d277136a47d9a3dbc8d29b3bfecc0fae69c89a518aae6`.
It was generated using the already bundled Pillow 12.3.0/OpenJPEG 2.5.4:

```python
image = Image.new('RGB', (8, 8))
image.putdata([(0, 255, 0) if x < 4 else (255, 0, 255)
               for y in range(8) for x in range(8)])
image.save(output, format='JPEG2000', irreversible=False)
```

No customer data, external fixture download, new dependency or paid service was
used. The browser must still validate the hand-authored fax encoding, font/CMap
behavior, pixel coordinates and asset/Worker observations. Static preparation
cannot establish browser compatibility or a passing result. If any assertion
fails, preserve that first result and diagnose the actual source/fixture cause;
do not substitute a render mock, relax CSP, grant eval/WASM permissions or turn
an unexercised asset into a claimed decoder pass.
