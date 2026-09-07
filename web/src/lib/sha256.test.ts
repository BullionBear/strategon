import { createHash } from 'node:crypto';
import { describe, expect, it, vi } from 'vitest';
import {
	AUTO_HASH_MAX_BYTES,
	CryptoUnavailableError,
	FileTooLargeToHashError,
	sha256File,
	webCryptoSubtle
} from './sha256';

function fileFrom(bytes: Uint8Array, name = 'blob.bin'): File {
	return new File([bytes], name);
}

describe('sha256File', () => {
	it('hashes via streamed chunks, not arrayBuffer', async () => {
		const payload = new TextEncoder().encode('hello-strategon');
		const file = fileFrom(payload);
		const arrayBuffer = vi.spyOn(file, 'arrayBuffer');
		const digest = await sha256File(file);
		const want = `sha256:${createHash('sha256').update(payload).digest('hex')}`;
		expect(digest).toBe(want);
		expect(arrayBuffer).not.toHaveBeenCalled();
	});

	it('rejects files over the auto-hash cap before reading', async () => {
		const file = fileFrom(new Uint8Array([1, 2, 3]));
		Object.defineProperty(file, 'size', { value: AUTO_HASH_MAX_BYTES + 1 });
		const stream = vi.spyOn(file, 'stream');
		await expect(sha256File(file)).rejects.toBeInstanceOf(FileTooLargeToHashError);
		expect(stream).not.toHaveBeenCalled();
	});

	it('errors clearly when crypto.subtle is missing', async () => {
		vi.stubGlobal('crypto', {});
		try {
			expect(webCryptoSubtle()).toBeUndefined();
			await expect(sha256File(fileFrom(new Uint8Array([1])))).rejects.toBeInstanceOf(
				CryptoUnavailableError
			);
		} finally {
			vi.unstubAllGlobals();
		}
	});
});
