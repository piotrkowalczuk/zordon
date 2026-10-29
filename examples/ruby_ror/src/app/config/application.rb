# frozen_string_literal: true

require_relative "boot"

require "rails"
require "active_record/railtie"
require "action_controller/railtie"

Bundler.require(*Rails.groups)

module RorExample
  class Application < Rails::Application
    config.load_defaults 8.1
    config.api_only = true
    config.eager_load = false

    # The app writes nothing into its own tree: logs go to stdout (alpha's
    # log), the database lives in fs::var() (DATABASE_PATH), migrations do
    # not dump db/schema.rb, and secret_key_base comes from the env instead
    # of a generated tmp/local_secret.txt.
    config.logger = ActiveSupport::Logger.new($stdout)
    config.active_record.dump_schema_after_migration = false
    config.cache_store = :null_store
    config.secret_key_base = ENV.fetch("SECRET_KEY_BASE")

    # zordon binds 127.0.0.1 on a picked port; the dev host allowlist would
    # otherwise reject requests that carry that port in the Host header.
    config.hosts.clear
  end
end
