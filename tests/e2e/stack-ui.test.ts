import {env} from 'node:process';
import {expect, test} from '@playwright/test';
import {apiCreateFiles, apiCreatePR, apiCreateRepo, login, randomString} from './utils.ts';

const owner = env.GITEA_TEST_E2E_USER;

test('stack pages create and render a pull request chain', async ({page, request}, testInfo) => {
  test.setTimeout(30_000);
  const repo = `e2e-stack-${randomString(8)}`;
  const createStack = (async () => {
    await apiCreateRepo(request, {name: repo});
    await apiCreateFiles(request, owner, repo, [{path: 'one.txt', content: 'one\n'}], {branch: 'main', newBranch: 'layer-one'});
    await apiCreateFiles(request, owner, repo, [{path: 'two.txt', content: 'two\n'}], {branch: 'layer-one', newBranch: 'layer-two'});
    await apiCreateFiles(request, owner, repo, [{path: 'three.txt', content: 'three\n'}], {branch: 'layer-two', newBranch: 'layer-three'});
    await apiCreateFiles(request, owner, repo, [{path: 'trunk.txt', content: 'trunk\n'}], {branch: 'main'});
    const one = await apiCreatePR(request, owner, repo, 'layer-one', 'main', 'Layer one');
    const two = await apiCreatePR(request, owner, repo, 'layer-two', 'layer-one', 'Layer two');
    const three = await apiCreatePR(request, owner, repo, 'layer-three', 'layer-two', 'Layer three');
    return {one, two, three};
  })();
  const [{two, three}] = await Promise.all([createStack, login(page)]);
  const stackURL = `/${owner}/${repo}/pulls/stacks`;
  await page.goto(`/${owner}/${repo}/pulls/${two}`);
  await page.screenshot({path: testInfo.outputPath('pull-before-stack.png'), fullPage: true});
  await page.goto(`${stackURL}/new`);
  await expect(page.getByRole('button', {name: 'Create stack'})).toBeDisabled();
  await expect(page.getByRole('button', {name: 'Find chain'})).toBeHidden();
  await page.getByLabel('Last pull request in the chain').pressSequentially('three');
  await expect(page.getByRole('option', {name: /Layer two/})).toBeHidden();
  await page.getByRole('option', {name: /Layer three/}).click();
  await expect(page).toHaveURL(new RegExp(`pull=${three}`));
  await expect(page.getByRole('radio', {name: /Layer one/})).toBeChecked();
  await expect(page.getByText('Lands into main')).toBeVisible();
  await expect(page.getByText('Behind main')).toBeVisible();
  await expect(page.getByRole('radio', {name: /^Rebase mode/})).toBeDisabled();
  await page.getByRole('radio', {name: /Layer two/}).check();
  await expect(page.getByText('Lands into layer-one')).toBeVisible();
  await expect(page.getByRole('radio', {name: /^Rebase mode/})).toBeEnabled();
  await page.getByRole('radio', {name: /Layer one/}).check();
  await expect(page.getByRole('radio', {name: /^Merge mode/})).toBeChecked();
  await page.screenshot({path: testInfo.outputPath('stack-new.png'), fullPage: true});
  await page.getByRole('button', {name: 'Create stack'}).click();
  await expect(page.getByRole('link', {name: /Stack #\d+/})).toBeVisible();

  await page.getByRole('link', {name: /Stack #\d+/}).click();
  await expect(page.getByRole('heading', {name: /Stack #\d+/})).toBeVisible();
  await expect(page.getByText('#1 Layer one')).toBeVisible();
  await expect(page.getByText('#2 Layer two')).toBeVisible();
  await expect(page.getByText('#3 Layer three')).toBeVisible();
  await expect(page.getByRole('button', {name: 'Update stack'})).toBeVisible();
  await expect(page.getByLabel('Merge method').locator('option')).toHaveText(['Create merge commit', 'Create squash commit', 'Fast-forward only']);
  await page.screenshot({path: testInfo.outputPath('stack-desktop.png'), fullPage: true});

  await page.setViewportSize({width: 375, height: 812});
  await expect.poll(() => page.evaluate(() => Math.max(document.documentElement.scrollWidth, document.scrollingElement!.scrollWidth) - innerWidth)).toBeLessThanOrEqual(0);
  await page.screenshot({path: testInfo.outputPath('stack-mobile.png'), fullPage: true});
  const pageWidth = await page.evaluate(() => ({document: document.documentElement.scrollWidth, scrolling: document.scrollingElement!.scrollWidth, viewport: innerWidth}));
  expect(pageWidth.document).toBeLessThanOrEqual(pageWidth.viewport);
  expect(pageWidth.scrolling).toBeLessThanOrEqual(pageWidth.viewport);

  await page.goto(`/${owner}/${repo}/pulls/${two}`);
  await expect(page.getByRole('heading', {name: /Part of Stack #\d+/})).toBeVisible();
  await expect(page.getByRole('link', {name: 'View stack'})).toBeVisible();
  await expect(page.locator('#pull-request-merge-form')).toHaveCount(0);
  await page.screenshot({path: testInfo.outputPath('pull-stack-mobile.png'), fullPage: true});
  await page.setViewportSize({width: 1280, height: 720});
  await page.screenshot({path: testInfo.outputPath('pull-after-stack.png'), fullPage: true});
});
