import {env} from 'node:process';
import {expect, test, type Locator} from '@playwright/test';
import {apiCreateFiles, apiCreatePR, apiCreateRepo, login, randomString} from './utils.ts';

const owner = env.GITEA_TEST_E2E_USER;

test('stack pages create and render a pull request chain', async ({page, request}, testInfo) => {
  test.setTimeout(30_000);
  const repo = `e2e-stack-${randomString(8)}`;
  const unsafeTitle = 'Unsafe <img src=x onerror="window.stackXss=1">';
  const expectEscapedSelection = async (dropdown: Locator) => {
    await dropdown.getByRole('combobox').pressSequentially('Unsafe');
    await dropdown.getByRole('option', {name: /Unsafe/}).click();
    await expect(dropdown.locator('.text')).toContainText(unsafeTitle);
    await expect(dropdown.locator('.text img')).toHaveCount(0);
    expect(await page.evaluate(() => 'stackXss' in window)).toBe(false);
  };
  const createStack = (async () => {
    await apiCreateRepo(request, {name: repo});
    await apiCreateFiles(request, owner, repo, [{path: 'one.txt', content: 'one\n'}], {branch: 'main', newBranch: 'layer-one'});
    await apiCreateFiles(request, owner, repo, [{path: 'two.txt', content: 'two\n'}], {branch: 'layer-one', newBranch: 'layer-two'});
    await apiCreateFiles(request, owner, repo, [{path: 'three.txt', content: 'three\n'}], {branch: 'layer-two', newBranch: 'layer-three'});
    await apiCreateFiles(request, owner, repo, [{path: 'solo.txt', content: 'solo\n'}], {branch: 'main', newBranch: 'solo'});
    await apiCreateFiles(request, owner, repo, [{path: 'trunk.txt', content: 'trunk\n'}], {branch: 'main'});
    const one = await apiCreatePR(request, owner, repo, 'layer-one', 'main', 'Layer one');
    const two = await apiCreatePR(request, owner, repo, 'layer-two', 'layer-one', 'Layer two');
    const three = await apiCreatePR(request, owner, repo, 'layer-three', 'layer-two', 'Layer three');
    await apiCreatePR(request, owner, repo, 'solo', 'main', 'Unstacked change');
    await apiCreateFiles(request, owner, repo, [{path: 'unsafe.txt', content: 'unsafe\n'}], {branch: 'layer-three', newBranch: 'unsafe'});
    const unsafe = await apiCreatePR(request, owner, repo, 'unsafe', 'layer-three', unsafeTitle);
    return {one, two, three, unsafe};
  })();
  const [{two, three, unsafe}] = await Promise.all([createStack, login(page)]);
  const stackURL = `/${owner}/${repo}/pulls/stacks`;
  await page.goto(`/${owner}/${repo}/pulls/${two}`);
  await page.screenshot({path: testInfo.outputPath('pull-before-stack.png'), fullPage: true});
  await page.goto(`${stackURL}/new?pull=${unsafe}`); // reselecting it doesn't reload the page
  await expectEscapedSelection(page.locator('.ui.dropdown', {has: page.getByLabel('Last pull request in the chain')}));
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
  // created after the stack so a shared base branch doesn't move the suggested start
  await apiCreateFiles(request, owner, repo, [{path: 'inserted.txt', content: 'inserted\n'}], {branch: 'layer-one', newBranch: 'layer-inserted'});
  const inserted = await apiCreatePR(request, owner, repo, 'layer-inserted', 'layer-one', 'Layer inserted');

  await page.getByRole('link', {name: /Stack #\d+/}).click();
  await expect(page.getByRole('heading', {name: /Stack #\d+/})).toBeVisible();
  await expect(page.getByText('#1 Layer one')).toBeVisible();
  await expect(page.getByText('#2 Layer two')).toBeVisible();
  await expect(page.getByText('#3 Layer three')).toBeVisible();
  await expect(page.getByRole('button', {name: 'Update stack'})).toBeVisible();
  await expect(page.getByLabel('Merge method').locator('option')).toHaveText(['Create merge commit', 'Create squash commit', 'Fast-forward only']);
  await page.screenshot({path: testInfo.outputPath('stack-desktop.png'), fullPage: true});

  const insertPicker = page.locator('.ui.dropdown', {has: page.getByRole('combobox', {name: 'Pull request'})});
  await expectEscapedSelection(insertPicker);
  await insertPicker.getByRole('combobox').pressSequentially('inserted');
  await expect(page.getByRole('option', {name: /Layer inserted.*inserted after #1/})).toBeVisible();
  await page.screenshot({path: testInfo.outputPath('stack-insert.png'), fullPage: true});
  await page.getByRole('option', {name: /Layer inserted/}).click();
  await page.getByRole('button', {name: 'Insert pull request'}).click();
  await expect(page.getByText(`Inserted #${inserted}. Select Update stack`)).toBeVisible();
  await expect(page.getByRole('list', {name: 'entries'}).getByRole('link')).toHaveText(['#1 Layer one', `#${inserted} Layer inserted`, '#2 Layer two', '#3 Layer three']);

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

  await page.goto(`/${owner}/${repo}/pulls?view=grouped`); // explicit, the preference is shared with parallel projects
  const layers = page.getByText('4 of 4 layers');
  const layerTwo = page.getByRole('checkbox', {name: /Layer two/});
  const unstacked = page.getByRole('checkbox', {name: /Unstacked change/});
  await expect(page.getByRole('link', {name: /^Stack #\d+$/})).toBeVisible();
  await expect(page.getByRole('img', {name: '0 merged, 4 open, 0 closed'})).toBeVisible();
  await expect(layerTwo).toBeHidden(); // more than three layers start collapsed
  await page.getByTitle('Check/Uncheck all items').check();
  await expect(unstacked).toBeChecked();
  await layers.click();
  await expect(layerTwo).not.toBeChecked(); // collapsed layers stay out of batch actions
  await unstacked.uncheck();
  await page.getByRole('checkbox', {name: /Unsafe/}).uncheck();
  await page.screenshot({path: testInfo.outputPath('pr-list-after.png'), fullPage: true});

  await page.setViewportSize({width: 375, height: 812});
  await expect.poll(() => page.evaluate(() => Math.max(document.documentElement.scrollWidth, document.scrollingElement!.scrollWidth) - innerWidth)).toBeLessThanOrEqual(0);
  await page.screenshot({path: testInfo.outputPath('pr-list-mobile.png'), fullPage: true});
  await page.setViewportSize({width: 1280, height: 720});

  await page.getByRole('link', {name: 'Flat'}).click();
  await expect(page.getByRole('link', {name: /^Stack #\d+ · 3\/4$/})).toBeVisible();
  await expect(page.getByRole('link', {name: 'Unstacked change'})).toBeVisible();
  await page.screenshot({path: testInfo.outputPath('pr-list-flat.png'), fullPage: true});
  await page.getByRole('link', {name: 'Grouped'}).click();
  await expect(page.getByRole('link', {name: /^Stack #\d+$/})).toBeVisible();
});
