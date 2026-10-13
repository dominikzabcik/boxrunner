const encoder = new TextEncoder();

export function base64url(bytes: Uint8Array): string {
	let binary = '';
	for (const byte of bytes) binary += String.fromCharCode(byte);
	return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export const base64urlJson = (value: unknown) => base64url(encoder.encode(JSON.stringify(value)));

const base64 = (bytes: Uint8Array) => {
	let binary = '';
	for (const byte of bytes) binary += String.fromCharCode(byte);
	return btoa(binary);
};

const fromBase64 = (text: string) => Uint8Array.from(atob(text), (char) => char.charCodeAt(0));

export const randomBytes = (length: number) => crypto.getRandomValues(new Uint8Array(length));

export const randomToken = () => base64url(randomBytes(32));

export async function sha256(text: string): Promise<Uint8Array> {
	return new Uint8Array(await crypto.subtle.digest('SHA-256', encoder.encode(text)));
}

export async function sha256Base64(text: string): Promise<string> {
	return base64(await sha256(text));
}

export const hex = (bytes: Uint8Array) => Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');

/** Compares SHA-256 digests, so neither the length nor the content of `given` leaks through timing. */
export async function sameSecret(given: string, expected: string): Promise<boolean> {
	const [a, b] = await Promise.all([sha256(given), sha256(expected)]);
	return sameDigest(a, b);
}

export function sameDigest(a: Uint8Array, b: Uint8Array): boolean {
	if (a.length !== b.length) return false;
	let difference = 0;
	for (let i = 0; i < a.length; i++) difference |= a[i]! ^ b[i]!;
	return difference === 0;
}

/** The box token is stored only as this hash; the token itself is shown once. */
export const boxTokenHash = async (token: string) => hex(await sha256(token));

export async function matchesBoxToken(presented: string, storedHash: string): Promise<boolean> {
	return sameDigest(encoder.encode(await boxTokenHash(presented)), encoder.encode(storedHash));
}

export async function hmac(secret: string, message: string): Promise<string> {
	const key = await crypto.subtle.importKey('raw', encoder.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, [
		'sign',
	]);
	return base64url(new Uint8Array(await crypto.subtle.sign('HMAC', key, encoder.encode(message))));
}

declare const appKeyBrand: unique symbol;
/** An RSA private key as base64 PKCS#8 DER, the only form WebCrypto imports. */
export type AppKey = string & { readonly [appKeyBrand]: true };

const rsaAlgorithm = { name: 'RSASSA-PKCS1-v1_5', hash: 'SHA-256' } as const;

function derLength(length: number): number[] {
	if (length < 0x80) return [length];
	const bytes: number[] = [];
	for (let rest = length; rest > 0; rest = Math.floor(rest / 256)) bytes.unshift(rest % 256);
	return [0x80 | bytes.length, ...bytes];
}

const der = (tag: number, content: Uint8Array) => Uint8Array.from([tag, ...derLength(content.length), ...content]);

// SEQUENCE { INTEGER 0, SEQUENCE { OID rsaEncryption, NULL } }, the PKCS#8 header for an RSA key.
const pkcs8RsaPrefix = [0x02, 0x01, 0x00, 0x30, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01, 0x05, 0x00];

/**
 * GitHub hands out the App key as PKCS#1 PEM. Wrapping it in a PKCS#8 envelope
 * lets WebCrypto import it; the key itself is not touched. The import at the
 * end proves the result is a usable signing key before anything is stored.
 */
export async function appKeyFromPem(pem: string): Promise<AppKey> {
	const match = /-----BEGIN (RSA )?PRIVATE KEY-----([\s\S]+?)-----END (RSA )?PRIVATE KEY-----/.exec(pem);
	if (!match) throw new Error('The App key is not a PEM private key');
	const body = fromBase64(match[2]!.replace(/\s+/g, ''));
	const pkcs8 = match[1] ? der(0x30, Uint8Array.from([...pkcs8RsaPrefix, ...der(0x04, body)])) : body;
	const key = base64(pkcs8) as AppKey;
	await importAppKey(key);
	return key;
}

const importAppKey = (key: AppKey) =>
	crypto.subtle.importKey('pkcs8', fromBase64(key), rsaAlgorithm, false, ['sign']);

/** GitHub accepts an App JWT for at most ten minutes; it is backdated a minute for clock drift. */
export async function signAppJwt(appId: number, key: AppKey, nowSeconds: number): Promise<string> {
	const unsigned = `${base64urlJson({ alg: 'RS256', typ: 'JWT' })}.${base64urlJson({
		iat: nowSeconds - 60,
		exp: nowSeconds + 570,
		iss: String(appId),
	})}`;
	const signature = await crypto.subtle.sign(rsaAlgorithm, await importAppKey(key), encoder.encode(unsigned));
	return `${unsigned}.${base64url(new Uint8Array(signature))}`;
}
