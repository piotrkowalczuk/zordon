# frozen_string_literal: true

class CreateVisits < ActiveRecord::Migration[8.1]
  def change
    create_table :visits, &:timestamps
  end
end
