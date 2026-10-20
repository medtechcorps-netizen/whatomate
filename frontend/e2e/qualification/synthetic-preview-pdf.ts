// Original synthetic fixture; no customer file, external fetch or PDF generator.
// Offsets and stream lengths are computed from bytes, not character counts.
export function syntheticPreviewPdf(): Buffer {
  const stream = (dictionary: string, bytes: Buffer) => Buffer.concat([
    Buffer.from(`<< ${dictionary} /Length ${bytes.length} >>\nstream\n`, 'ascii'),
    bytes,
    Buffer.from('\nendstream', 'ascii'),
  ])
  // CCITT Group3, one-dimensional, byte-aligned rows: white run4 (1011)
  // followed by black run4 (011), padded to one byte. Eight identical rows.
  // This must produce a white left half and black right half when rendered;
  // a dropped/undecoded image cannot satisfy the black-pixel oracle.
  const fax = Buffer.alloc(8, 0b10110110)
  // Original lossless RGB 8x8 JP2: left green, right magenta. Generated once
  // with bundled Pillow 12.3.0/OpenJPEG 2.5.4; no runtime generator dependency.
  // 294 bytes, SHA256 7d2278c692e8eb08410d277136a47d9a3dbc8d29b3bfecc0fae69c89a518aae6.
  const jpx = Buffer.from(
    'AAAADGpQICANCocKAAAAFGZ0eXBqcDIgAAAAAGpwMiAAAAAtanAyaAAAABZpaGRyAAAACAAAAAgAAwcHAAAAAAAPY29scgEAAAAAABAAAADZanAyY/9P/1EALwAAAAAACAAAAAgAAAAAAAAAAAAAAAgAAAAIAAAAAAAAAAAAAwcBAQcBAQcBAf9SAAwAAAABAAMEBAAB/1wADUBASEhQSEhQSEhQ/2QAJQABQ3JlYXRlZCBieSBPcGVuSlBFRyB2ZXJzaW9uIDIuNS40/5AACgAAAAAAWAAB/5PPtAgHP8+0CASfz7QIBz/fmBAEk9+YEAc/35gQBJPPwBQJQLM658/AFAKIej1/z8AUCUCzOufH2gYAFyu8z8AMF9i0x9oGABcrvP/Z',
    'base64',
  )
  const objects: Array<string | Buffer> = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R 10 0 R] /Count 2 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /FS 5 0 R /FJ 6 0 R >> /XObject << /Fax 9 0 R >> >> /Contents 4 0 R >>',
    stream('', Buffer.from([
      '1 0 0 rg 10 10 50 50 re f',
      '0 0 0 rg BT /FS 30 Tf 10 150 Td (A) Tj ET',
      'BT /FJ 24 Tf 100 150 Td <3042> Tj ET',
      'q 80 0 0 80 100 20 cm /Fax Do Q',
    ].join('\n'), 'ascii')),
    '<< /Type /Font /Subtype /Type1 /BaseFont /Symbol >>',
    '<< /Type /Font /Subtype /Type0 /BaseFont /HeiseiMin-W3 /Encoding /UniJIS-UCS2-H /DescendantFonts [7 0 R] >>',
    '<< /Type /Font /Subtype /CIDFontType0 /BaseFont /HeiseiMin-W3 /CIDSystemInfo << /Registry (Adobe) /Ordering (Japan1) /Supplement 6 >> /FontDescriptor 8 0 R /DW 1000 >>',
    '<< /Type /FontDescriptor /FontName /HeiseiMin-W3 /Flags 4 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 880 /Descent -120 /CapHeight 700 /StemV 80 >>',
    stream('/Type /XObject /Subtype /Image /Width 8 /Height 8 /ColorSpace /DeviceGray /BitsPerComponent 1 /Filter /CCITTFaxDecode /DecodeParms << /K 0 /Columns 8 /Rows 8 /EndOfLine false /EncodedByteAlign true /EndOfBlock false /BlackIs1 false >>', fax),
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /FZ 13 0 R >> /XObject << /JPX 12 0 R >> >> /Contents 11 0 R >>',
    stream('', Buffer.from('0 0 1 rg 10 10 50 50 re f\n0 0 0 rg BT /FZ 30 Tf 10 150 Td (4) Tj ET\nq 80 0 0 80 100 20 cm /JPX Do Q', 'ascii')),
    stream('/Type /XObject /Subtype /Image /Width 8 /Height 8 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /JPXDecode', jpx),
    '<< /Type /Font /Subtype /Type1 /BaseFont /ZapfDingbats >>',
  ]
  const chunks = [Buffer.from('%PDF-1.7\n% synthetic R2 preview\n', 'ascii')]
  const offsets = [0]
  let length = chunks[0].length
  objects.forEach((body, index) => {
    offsets.push(length)
    const object = Buffer.concat([Buffer.from(`${index + 1} 0 obj\n`, 'ascii'),
      typeof body === 'string' ? Buffer.from(body, 'ascii') : body,
      Buffer.from('\nendobj\n', 'ascii')])
    chunks.push(object)
    length += object.length
  })
  const xref = length
  chunks.push(Buffer.from(`xref\n0 ${offsets.length}\n0000000000 65535 f \n` +
    offsets.slice(1).map(offset => `${String(offset).padStart(10, '0')} 00000 n \n`).join('') +
    `trailer\n<< /Size ${offsets.length} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`, 'ascii'))
  return Buffer.concat(chunks)
}
