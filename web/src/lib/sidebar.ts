const STORAGE_KEY = 'strategon.sidebarCollapsed';

export function hasSidebarCollapsedPreference(): boolean {
	if (typeof localStorage === 'undefined') return false;
	try {
		return localStorage.getItem(STORAGE_KEY) != null;
	} catch {
		return false;
	}
}

export function readSidebarCollapsed(): boolean {
	if (typeof localStorage === 'undefined') return false;
	try {
		return localStorage.getItem(STORAGE_KEY) === '1';
	} catch {
		return false;
	}
}

export function writeSidebarCollapsed(collapsed: boolean): void {
	if (typeof localStorage === 'undefined') return;
	try {
		localStorage.setItem(STORAGE_KEY, collapsed ? '1' : '0');
	} catch {
		/* ignore quota / private mode */
	}
}
