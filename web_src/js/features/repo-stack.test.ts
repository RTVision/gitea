import {GET} from '../modules/fetch.ts';
import {initRepoStackStatus} from './repo-stack.ts';

vi.mock('../modules/fetch.ts', () => ({GET: vi.fn()}));

beforeEach(() => {
	vi.clearAllMocks();
  document.body.innerHTML = '<div data-stack-status-url="/status" data-reloading-interval="10">existing</div>';
  vi.useFakeTimers();
});

afterEach(() => vi.useRealTimers());

test('retries a rejected status request and preserves the current fragment', async () => {
  vi.mocked(GET).mockRejectedValueOnce(new Error('offline')).mockResolvedValueOnce({ok: true, text: () => Promise.resolve('<div data-status-final="true">done</div>')} as Response);

  initRepoStackStatus();
	await Promise.resolve();

	expect(document.body.textContent).toContain('existing');
	expect(GET).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(10);

  expect(GET).toHaveBeenCalledTimes(2);
  expect(document.body.textContent).toContain('done');
});

test('stops polling after a terminal status response', async () => {
  vi.mocked(GET).mockResolvedValue({ok: true, text: () => Promise.resolve('<div data-status-final="true">done</div>')} as Response);

  initRepoStackStatus();
  await vi.advanceTimersByTimeAsync(100);

  expect(GET).toHaveBeenCalledTimes(1);
});
