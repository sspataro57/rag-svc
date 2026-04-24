// Thin wrapper over chrome.storage.sync so content script + service worker +
// options page share one contract. Synced across the user's Chrome profile.

export interface Settings {
  backendUrl: string;
}

const DEFAULTS: Settings = {
  backendUrl: "",
};

export async function getSettings(): Promise<Settings> {
  const raw = await chrome.storage.sync.get(DEFAULTS);
  return { ...DEFAULTS, ...raw } as Settings;
}

export async function setSettings(s: Partial<Settings>): Promise<void> {
  await chrome.storage.sync.set(s);
}
