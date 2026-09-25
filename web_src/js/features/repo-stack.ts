import {GET} from '../modules/fetch.ts';
import {registerGlobalInitFunc} from '../modules/observer.ts';
import {hideElem, toggleElem, toggleElemClass} from '../utils/dom.ts';

export function initRepoStackNew() {
  registerGlobalInitFunc('initRepoStackNew', (form: HTMLFormElement) => {
    const findChain = form.querySelector<HTMLButtonElement>('.stack-new-find')!;
    hideElem(findChain); // only needed without JS
    form.querySelector('input[name="pull"]')!.addEventListener('change', () => {
      form.querySelector('.stack-new-layers')!.classList.add('is-loading');
      form.requestSubmit(findChain);
    });
    const layers = Array.from(form.querySelectorAll<HTMLElement>('.stack-new-layer'));
    const merge = form.querySelector<HTMLInputElement>('input[name="mode"][value="merge"]')!;
    const rebase = form.querySelector<HTMLInputElement>('input[name="mode"][value="rebase"]')!;
    const syncStart = () => {
      const start = layers.findIndex((layer) => layer.querySelector<HTMLInputElement>('input[name="start"]')!.checked);
      const joined = layers.slice(start);
      for (const [i, layer] of layers.entries()) toggleElemClass(layer.querySelector('.checkbox')!, 'tw-opacity-50', i < start);
      form.querySelector('.stack-new-trunk')!.textContent = layers[start].getAttribute('data-base');
      rebase.disabled = joined.some((layer) => layer.hasAttribute('data-rebase-blocked'));
      if (rebase.disabled && rebase.checked) merge.checked = true;
      toggleElem(form.querySelector('.stack-new-rebase-blocked')!, rebase.disabled);
      form.querySelector<HTMLButtonElement>('.stack-new-create')!.disabled = joined.some((layer) => layer.hasAttribute('data-invalid'));
    };
    for (const layer of layers) layer.querySelector('input[name="start"]')!.addEventListener('change', syncStart);
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
