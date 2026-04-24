import { getSettings, setSettings } from "../storage";

const urlInput = document.getElementById("backendUrl") as HTMLInputElement;
const statusEl = document.getElementById("status") as HTMLSpanElement;
const extIdEl = document.getElementById("extId") as HTMLElement;
const saveBtn = document.getElementById("save") as HTMLButtonElement;

// Runtime ID is assigned by Chrome when the extension loads; with the
// manifest `key` pinned it's stable across reinstalls.
extIdEl.textContent = chrome.runtime.id;

void (async () => {
  const settings = await getSettings();
  urlInput.value = settings.backendUrl;
})();

saveBtn.addEventListener("click", async () => {
  const value = urlInput.value.trim();
  if (value && !/^https?:\/\//i.test(value)) {
    statusEl.textContent = "Must start with http:// or https://";
    statusEl.style.color = "#b91c1c";
    return;
  }
  await setSettings({ backendUrl: value });
  statusEl.style.color = "#0f766e";
  statusEl.textContent = "Saved.";
  setTimeout(() => (statusEl.textContent = ""), 2000);
});

// Save on Enter.
urlInput.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter") saveBtn.click();
});
