# Every GET / records a visit, so the count proves the database zordon's
# `db` provision prepared is the one the server talks to.
class VisitsController < ApplicationController
  def create
    Visit.create!
    render json: {
      visits: Visit.count,
      rails: Rails.version,
      ruby: RUBY_VERSION,
      bundler: Bundler::VERSION,
    }
  end
end
