import { redirect } from '@sveltejs/kit';

/** Legacy path — tokens are on /tokens; docs are on /reference. */
export function load() {
	redirect(301, '/tokens');
}
