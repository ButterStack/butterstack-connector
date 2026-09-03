# frozen_string_literal: true

# Generates a throwaway CA and server certificate for the drill harness, so the
# connector dials a real wss:// endpoint with real certificate verification.
#
# The connector has no option to skip verification for the broker connection.
# That is the point of doing this rather than testing over plaintext: if the
# daemon could be talked into an unverified broker connection, the drills would
# not be exercising the transport the design promises.
require 'openssl'
require 'fileutils'

module MockTLS
  Bundle = Struct.new(:ca_pem_path, :server_cert, :server_key, keyword_init: true)

  def self.generate(dir, hostname = 'localhost')
    FileUtils.mkdir_p(dir)

    ca_key = OpenSSL::PKey::RSA.new(2048)
    ca_cert = OpenSSL::X509::Certificate.new
    ca_cert.version = 2
    ca_cert.serial = 1
    ca_cert.subject = OpenSSL::X509::Name.parse('/CN=butterstack-connector-drill-ca')
    ca_cert.issuer = ca_cert.subject
    ca_cert.public_key = ca_key.public_key
    ca_cert.not_before = Time.now - 3600
    ca_cert.not_after = Time.now + 86_400
    ef = OpenSSL::X509::ExtensionFactory.new
    ef.subject_certificate = ca_cert
    ef.issuer_certificate = ca_cert
    ca_cert.add_extension(ef.create_extension('basicConstraints', 'CA:TRUE', true))
    ca_cert.add_extension(ef.create_extension('keyUsage', 'keyCertSign,cRLSign', true))
    ca_cert.sign(ca_key, OpenSSL::Digest::SHA256.new)

    key = OpenSSL::PKey::RSA.new(2048)
    cert = OpenSSL::X509::Certificate.new
    cert.version = 2
    cert.serial = 2
    cert.subject = OpenSSL::X509::Name.parse("/CN=#{hostname}")
    cert.issuer = ca_cert.subject
    cert.public_key = key.public_key
    cert.not_before = Time.now - 3600
    cert.not_after = Time.now + 86_400
    ef2 = OpenSSL::X509::ExtensionFactory.new
    ef2.subject_certificate = cert
    ef2.issuer_certificate = ca_cert
    cert.add_extension(ef2.create_extension('basicConstraints', 'CA:FALSE', true))
    cert.add_extension(ef2.create_extension('keyUsage', 'digitalSignature,keyEncipherment', true))
    cert.add_extension(ef2.create_extension('extendedKeyUsage', 'serverAuth'))
    cert.add_extension(ef2.create_extension('subjectAltName', "DNS:#{hostname},IP:127.0.0.1"))
    cert.sign(ca_key, OpenSSL::Digest::SHA256.new)

    ca_path = File.join(dir, 'drill-ca.pem')
    File.write(ca_path, ca_cert.to_pem)
    File.chmod(0o600, ca_path)

    Bundle.new(ca_pem_path: ca_path, server_cert: cert, server_key: key)
  end
end
