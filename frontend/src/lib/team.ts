// Only the selected team ID is persisted. It is not a credential: the server authorizes every request against the session.
const storageKey = "cove.team";

let selected: string | null = read();

function read() {
  try {
    return window.localStorage.getItem(storageKey);
  } catch {
    return null;
  }
}

export function selectedTeam() {
  return selected;
}

export function rememberTeam(teamId: string | null) {
  selected = teamId;
  try {
    if (teamId) window.localStorage.setItem(storageKey, teamId);
    else window.localStorage.removeItem(storageKey);
  } catch {
    // Storage can be unavailable (private windows, blocked site data); the selection then lasts for this page load.
  }
}
