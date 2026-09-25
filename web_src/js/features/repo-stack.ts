import {GET} from '../modules/fetch.ts';
import {fomanticQuery} from '../modules/fomantic/base.ts';
import {registerGlobalInitFunc} from '../modules/observer.ts';

export function initRepoStackNew() {
  registerGlobalInitFunc('initRepoStackNew', (form: HTMLFormElement) => {
    form.querySelector<HTMLSelectElement>('select[name="pull"]')!.addEventListener('change', () => form.submit());
  });
}

export function initRepoStackInsert() {
  registerGlobalInitFunc('initRepoStackInsert', (form: HTMLFormElement) => {
    const submit = form.querySelector<HTMLButtonElement>('button[type="submit"]');
    if (!submit) return; // no candidates to pick
    fomanticQuery(form.querySelector('.ui.dropdown')!).dropdown('setting', {
      fullTextSearch: true, // match titles and branches, not just number prefixes
      onChange: () => { submit.disabled = false },
    });
  });
}

export function initRepoStackStatus() {
  for (const status of document.querySelectorAll<HTMLElement>('[data-stack-status-url]')) {
    if (status.getAttribute('data-stack-status-polling')) continue;
    status.setAttribute('data-stack-status-polling', 'true');
    const refresh = async () => {
      const response = await GET(status.getAttribute('data-stack-status-url')!);
      if (!response.ok) return true;
      status.innerHTML = await response.text();
      return !status.querySelector('[data-status-final="true"]');
    };
    const poll = async () => {
      if (!status.isConnected) return;
      let retry = true;
      try {
        retry = await refresh();
      } catch {
        // Keep the current fragment while a transient network failure recovers.
      }
      if (retry && status.isConnected) setTimeout(poll, Number(status.getAttribute('data-reloading-interval')));
    };
    poll(); // no await
  }
}
