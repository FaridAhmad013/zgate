const fs = require('fs');
const os = require('os');
const crypto = require('crypto');
const path = require('path');

try {
  const credPath = path.join(os.homedir(), '.zcode', 'v2', 'credentials.json');
  if (!fs.existsSync(credPath)) {
    process.exit(0);
  }
  const raw = fs.readFileSync(credPath, 'utf8');
  const data = JSON.parse(raw);
  const secret = 'zcode-credential-fallback:' + os.platform() + ':' + os.homedir() + ':' + os.userInfo().username;
  const key = crypto.createHash('sha256').update(secret).digest();

  function decrypt(str) {
    if (!str.startsWith('enc:v1:')) return str;
    const [ivB64, tagB64, dataB64] = str.slice('enc:v1:'.length).split('.');
    const iv = Buffer.from(ivB64, 'base64url');
    const tag = Buffer.from(tagB64, 'base64url');
    const ciphertext = Buffer.from(dataB64, 'base64url');
    const decipher = crypto.createDecipheriv('aes-256-gcm', key, iv);
    decipher.setAuthTag(tag);
    return Buffer.concat([decipher.update(ciphertext), decipher.final()]).toString('utf8');
  }

  if (data['zcodejwttoken']) {
    const jwt = decrypt(data['zcodejwttoken']);
    const target = path.join(__dirname, 'plan-jwt.json');
    fs.writeFileSync(target, JSON.stringify({ jwt }), { encoding: 'utf8', mode: 0o600 });
    try { fs.chmodSync(target, 0o600); } catch (_) {}
    console.log('[sync-token] Token synced successfully');
  }
} catch (e) {
  const msg = String((e && e.message) || 'unknown error').replace(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*/g, '[REDACTED_JWT]').replace(/enc:v1:[A-Za-z0-9_.-]+/g, 'enc:v1:[REDACTED]');
  console.error('[sync-token] Error syncing token:', msg);
}
