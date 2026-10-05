import { mkdir, rename } from 'node:fs/promises';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const modulePath = process.env.CODEX_PRIMARY_RUNTIME_NODE_MODULES
  ? pathToFileURL(path.join(process.env.CODEX_PRIMARY_RUNTIME_NODE_MODULES, 'playwright', 'index.mjs')).href
  : 'playwright';
const { chromium } = await import(modulePath);
const replay = process.argv[2];
if (!replay) throw new Error('usage: npm run capture -- runs/cf-*/replay.html [evidence-dir]');
const out = path.resolve(process.argv[3] || 'evidence');
await mkdir(out, { recursive: true });
const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined });
const context = await browser.newContext({ viewport: { width: 1600, height: 1300 }, recordVideo: { dir: out, size: { width: 1600, height: 1300 } } });
const page = await context.newPage();
const errors = [];
page.on('pageerror', e => errors.push(String(e)));
await page.goto(pathToFileURL(path.resolve(replay)).href);
await page.waitForFunction(() => document.querySelector('#status').textContent === 'completed');
await page.locator('#backend-node').click();
await page.screenshot({ path: path.join(out, 'dashboard.png'), fullPage: true });
if (!(await page.locator('#detail').textContent()).includes('checkpoint reused')) throw new Error('resume evidence missing');
await page.locator('#unit').click();
if (!(await page.locator('#detail').textContent()).includes('FAIL')) throw new Error('first test failure missing');
await page.locator('#review_final').click();
if (!(await page.locator('#detail').textContent()).includes('APPROVED')) throw new Error('fresh approval evidence missing');
await page.locator('#backend-node').click();
await page.locator('#play').click();
await page.waitForFunction(() => {
  const slider = document.querySelector('#seek');
  return +slider.value === +slider.max;
}, undefined, { timeout: 60000 });
await page.locator('#review_final').click();
await page.screenshot({ path: path.join(out, 'dashboard-final.png'), fullPage: true });
const video = page.video();
await context.close();
await rename(await video.path(), path.join(out, 'demo.webm'));
const mobile = await browser.newPage({viewport:{width:390,height:844}});
await mobile.goto(pathToFileURL(path.resolve(replay)).href);
await mobile.waitForFunction(() => document.querySelector('#status').textContent === 'completed');
await mobile.screenshot({path:path.join(out,'dashboard-mobile.png'),fullPage:true});
if (await mobile.evaluate(() => document.documentElement.scrollWidth>window.innerWidth)) throw new Error('mobile page overflows');
await mobile.close();
await browser.close();
if (errors.length) throw new Error(errors.join('\n'));
console.log('PASS: dashboard renders, failure/approval/checkpoint evidence visible, replay completes, no browser errors');
