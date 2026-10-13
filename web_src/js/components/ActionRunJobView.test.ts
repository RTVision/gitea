import {createApp, h} from 'vue';
import ActionRunJobView from './ActionRunJobView.vue';
import {createActionRunViewStore, createEmptyActionsRun} from './ActionRunView.ts';
import {POST} from '../modules/fetch.ts';

vi.mock('../modules/fetch.ts', () => ({POST: vi.fn()}));

for (const done of [true, false]) {
  test(`reloads changed step logs with polling ${done ? 'stopped' : 'active'}`, async () => {
    vi.useFakeTimers();
    const startPolling = vi.spyOn(globalThis, 'setInterval');
    const stopPolling = vi.spyOn(globalThis, 'clearInterval');
    const run = {...createEmptyActionsRun(), done, status: 'success' as const};
    const build = {summary: 'Build', duration: '1s', status: 'success' as const};
    const setup = {...build, summary: 'Set up'};
    const response = (steps: typeof build[], stepsLog: unknown[] = []) => Response.json({
      artifacts: [], state: {run, currentJob: {title: 'Build', detail: '', steps}}, logs: {stepsLog},
    });
    vi.mocked(POST).mockReset()
      .mockResolvedValueOnce(response([build]))
      .mockResolvedValueOnce(response([build]))
      .mockResolvedValueOnce(response([setup, build], [{step: 0, cursor: 'eof', lines: [{index: 0, timestamp: 1, message: 'stale logs'}]}]))
      .mockResolvedValue(response([setup, build], [{step: 1, cursor: 'eof', lines: [{index: 0, timestamp: 1, message: 'fresh logs'}]}]));
    const root = document.createElement('div');
    document.body.append(root);
    const app = createApp({render: () => h(ActionRunJobView, {
      store: createActionRunViewStore('/run'), jobId: 1, actionsViewUrl: '/job',
      locale: {
        status: {unknown: '', waiting: '', running: '', cancelling: '', success: '', failure: '', cancelled: '', skipped: '', blocked: '', pending: ''},
        showTimeStamps: '', showLogSeconds: '', showFullScreen: '', logsAlwaysAutoScroll: '',
        logsAlwaysExpandRunning: '', downloadLogs: '', copyOutput: '',
      },
    })});
    try {
      app.mount(root);
      await vi.waitFor(() => expect(startPolling).toHaveBeenCalledTimes(1));
      await vi.advanceTimersByTimeAsync(1000);
      expect(POST).toHaveBeenCalledTimes(2);
      if (done) await vi.waitFor(() => expect(stopPolling).toHaveBeenCalledTimes(1));
      root.querySelector<HTMLElement>('.job-step-summary')!.click();
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(1000);
      await vi.waitFor(() => expect(root.textContent).toContain('fresh logs'));
      expect(POST).toHaveBeenLastCalledWith('/job', expect.objectContaining({data: {logCursors: [
        {step: 0, cursor: null, expanded: false}, {step: 1, cursor: null, expanded: true},
      ]}}));
      expect(root.textContent).not.toContain('stale logs');
    } finally {
      app.unmount();
      root.remove();
      startPolling.mockRestore();
      stopPolling.mockRestore();
      vi.useRealTimers();
    }
  });
}
